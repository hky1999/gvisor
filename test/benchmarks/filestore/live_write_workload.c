// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

#include <errno.h>
#include <fcntl.h>
#include <signal.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

#define BLOCK_BYTES (1U << 20)
#define RECORD_MAGIC 0x475649534f524c57ULL

static _Noreturn void fail(const char* what) {
  perror(what);
  exit(1);
}

static uint64_t number(const char* text) {
  char* end;
  errno = 0;
  unsigned long long value = strtoull(text, &end, 10);
  if (errno || !*text || *text == '-' || *end) exit(2);
  return value;
}

static uint64_t now_ns(void) {
  struct timespec time;
  if (clock_gettime(CLOCK_MONOTONIC, &time)) fail("clock_gettime");
  return (uint64_t)time.tv_sec * 1000000000ULL + (uint64_t)time.tv_nsec;
}

static const char* payload_path(void) {
  const char* path = getenv("FILESTORE_BENCH_PAYLOAD");
  return path ? path : "/payload";
}

static void transfer(int fd, unsigned char* data, size_t length,
                     uint64_t offset, int writing) {
  while (length) {
    ssize_t done = writing ? pwrite(fd, data, length, (off_t)offset)
                           : pread(fd, data, length, (off_t)offset);
    if (done < 0 && errno == EINTR) continue;
    if (done <= 0 || (size_t)done > length) fail("payload transfer");
    data += done;
    length -= (size_t)done;
    offset += (uint64_t)done;
  }
}

static void fill_record(unsigned char* data, uint64_t block,
                        uint64_t generation) {
  uint64_t* words = (uint64_t*)data;
  uint64_t state = (0x9e3779b97f4a7c15ULL ^ block * 0xd6e8feb86659fd93ULL ^
                    generation * 0xa0761d6478bd642fULL) |
                   1;
  for (size_t i = 0; i < BLOCK_BYTES / sizeof(uint64_t); i++) {
    state ^= state << 13;
    state ^= state >> 7;
    state ^= state << 17;
    words[i] = state;
  }
  words[0] = RECORD_MAGIC;
  words[1] = block;
  words[2] = generation;
  words[3] = BLOCK_BYTES;
}

static uint64_t generation_for(uint64_t block, uint64_t hot_blocks,
                               uint64_t dirty_bytes, uint64_t sequence) {
  if (block < hot_blocks && sequence >= block + 1) {
    uint64_t latest = sequence - (sequence - block - 1) % hot_blocks;
    return latest + 1;
  }
  return block * BLOCK_BYTES < dirty_bytes ? 1 : 0;
}

static int check_size(int fd, uint64_t size) {
  struct stat info;
  if (fstat(fd, &info)) fail("fstat");
  return (uint64_t)info.st_size != size;
}

int main(int argc, char** argv) {
  if (argc == 2 && !strcmp(argv[1], "ready"))
    return access("/ready", F_OK) != 0;
  if (argc < 3) return 2;
  uint64_t size = number(argv[2]);
  if (!size || size > INT64_MAX || size % BLOCK_BYTES) return 2;
  int serve = !strcmp(argv[1], "serve");
  int init = !strcmp(argv[1], "init");
  int dirty = !strcmp(argv[1], "dirty");
  int verify = !strcmp(argv[1], "verify");
  if ((!serve && !init && !dirty && !verify) || (serve && argc != 4) ||
      (init && argc != 3) || (dirty && argc != 4) || (verify && argc != 6))
    return 2;
  uint64_t hot = serve || verify ? number(argv[3]) : size;
  uint64_t dirty_bytes = dirty ? number(argv[3]) : verify ? number(argv[4]) : 0;
  uint64_t sequence = verify ? number(argv[5]) : 0;
  if (!hot || hot > size || hot % BLOCK_BYTES || dirty_bytes > size ||
      dirty_bytes % BLOCK_BYTES)
    return 2;
  int fd =
      open(payload_path(),
           verify ? O_RDONLY : O_RDWR | (dirty ? 0 : O_CREAT | O_TRUNC), 0600);
  if (fd < 0) fail("payload open");
  if ((dirty || verify) && check_size(fd, size)) return 2;
  unsigned char* buffer = malloc(BLOCK_BYTES);
  unsigned char* actual = verify ? malloc(BLOCK_BYTES) : NULL;
  if (!buffer || (verify && !actual)) fail("malloc");
  uint64_t limit = dirty ? dirty_bytes : size;
  for (uint64_t off = 0; off < limit; off += BLOCK_BYTES) {
    uint64_t block = off / BLOCK_BYTES;
    uint64_t generation =
        verify ? generation_for(block, hot / BLOCK_BYTES, dirty_bytes, sequence)
        : dirty ? 1
                : 0;
    fill_record(buffer, block, generation);
    if (verify) {
      transfer(fd, actual, BLOCK_BYTES, off, 0);
      if (memcmp(actual, buffer, BLOCK_BYTES)) {
        fprintf(stderr,
                "version mismatch: block=%llu expected_generation=%llu "
                "sequence=%llu\n",
                (unsigned long long)block, (unsigned long long)generation,
                (unsigned long long)sequence);
        return 1;
      }
    } else {
      transfer(fd, buffer, BLOCK_BYTES, off, 1);
    }
  }
  if (verify || dirty || init) {
    if (init && fsync(fd)) fail("initial fsync");
    if (close(fd)) fail("payload close");
    free(actual);
    free(buffer);
    return 0;
  }
  if (fsync(fd)) fail("initial guest fsync");
  // The heartbeat does not share the writer's execution or rate limiter.
  pid_t heartbeat = fork();
  if (heartbeat < 0) fail("heartbeat fork");
  if (!heartbeat) {
    close(fd);
    close(STDIN_FILENO);
    struct timespec delay = {.tv_sec = 0, .tv_nsec = 1000000};
    for (uint64_t count = 0;; count++) {
      if (dprintf(STDOUT_FILENO, "H %llu %llu\n", (unsigned long long)count,
                  (unsigned long long)now_ns()) < 0)
        fail("heartbeat output");
      nanosleep(&delay, NULL);
    }
  }
  const char* ready = getenv("FILESTORE_BENCH_READY");
  if (!ready) ready = "/ready";
  int ready_fd = open(ready, O_WRONLY | O_CREAT | O_EXCL, 0600);
  if (ready_fd < 0 || close(ready_fd)) fail("ready");
  sequence = 0;
  for (;;) {
    char credit;
    ssize_t count = read(STDIN_FILENO, &credit, 1);
    if (count < 0 && errno == EINTR) continue;
    if (count <= 0 || credit == 'q') break;
    if (credit == 'b') {
      if (dprintf(STDOUT_FILENO, "Z %llu\n", (unsigned long long)sequence) < 0)
        fail("barrier output");
      continue;
    }
    if (credit != 'w') return 2;
    sequence++;
    uint64_t block = (sequence - 1) % (hot / BLOCK_BYTES);
    fill_record(buffer, block, sequence + 1);
    if (dprintf(STDOUT_FILENO, "B %llu\n", (unsigned long long)sequence) < 0)
      fail("begin output");
    uint64_t start = now_ns();
    transfer(fd, buffer, BLOCK_BYTES, block * BLOCK_BYTES, 1);
    uint64_t end = now_ns();
    if (dprintf(STDOUT_FILENO, "E %llu %llu %llu\n",
                (unsigned long long)sequence, (unsigned long long)start,
                (unsigned long long)end) < 0)
      fail("completion output");
  }
  kill(heartbeat, SIGTERM);
  waitpid(heartbeat, NULL, 0);
  if (close(fd)) fail("writer close");
  free(buffer);
  return 0;
}
