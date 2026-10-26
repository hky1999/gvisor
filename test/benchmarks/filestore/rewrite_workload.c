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

// Populate a complete nonzero layer, rewrite selected pages, and verify every
// byte after restore. Eight PRNG bytes per step keep preparation and
// verification outside checkpoint timing inexpensive, without repeating a small
// data block.
#include <errno.h>
#include <fcntl.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>

static _Noreturn void fail(const char* what) {
  perror(what);
  exit(1);
}

static uint64_t number(const char* text) {
  char* end;
  errno = 0;
  unsigned long long value = strtoull(text, &end, 10);
  if (errno || !*text || *text == '-' || *end) {
    fprintf(stderr, "invalid byte count: %s\n", text);
    exit(2);
  }
  return value;
}

static void transfer(int fd, unsigned char* data, size_t length,
                     uint64_t offset, int writing) {
  while (length) {
    ssize_t done = writing ? pwrite(fd, data, length, (off_t)offset)
                           : pread(fd, data, length, (off_t)offset);
    if (done < 0 && errno == EINTR) continue;
    if (done <= 0 || (size_t)done > length) fail(writing ? "pwrite" : "pread");
    data += done;
    length -= (size_t)done;
    offset += (uint64_t)done;
  }
}

static uint64_t target_page(uint64_t index, uint64_t total_pages,
                            uint64_t changed_pages, int spread) {
  return spread ? index * total_pages / changed_pages : index;
}

int main(int argc, char** argv) {
  if (argc < 2) return 2;
  if (!strcmp(argv[1], "ready")) return access("/ready", F_OK) != 0;
  if (argc < 3 || argc > 5) return 2;
  int verify = !strcmp(argv[1], "verify");
  int rewrite = !strcmp(argv[1], "rewrite");
  int serve = !strcmp(argv[1], "serve");
  int write_only = !strcmp(argv[1], "write");
  if (!verify && !rewrite && !serve && !write_only) return 2;
  uint64_t size = number(argv[2]);
  uint64_t changed = argc >= 4 ? number(argv[3]) : 0;
  int spread = argc == 5 && !strcmp(argv[4], "spread");
  if (argc == 5 && !spread && strcmp(argv[4], "prefix")) return 2;
  size_t page_size = (size_t)sysconf(_SC_PAGESIZE);
  if (size > INT64_MAX || changed > size || size % page_size ||
      changed % page_size || page_size > 65536 || 65536 % page_size)
    return 2;
  const char* path = getenv("FILESTORE_BENCH_PAYLOAD");
  if (!path) path = "/payload";
  int fd = open(path,
                verify    ? O_RDONLY
                : rewrite ? O_RDWR
                          : O_RDWR | O_CREAT | O_TRUNC,
                0600);
  if (fd < 0) fail("payload");
  uint64_t total_pages = size / page_size;
  uint64_t changed_pages = changed / page_size;
  unsigned char actual[65536];
  uint64_t words[65536 / sizeof(uint64_t)];
  if (rewrite) {
    struct stat info;
    if (fstat(fd, &info)) fail("fstat");
    if ((uint64_t)info.st_size != size) return 2;
    for (uint64_t i = 0; i < changed_pages; i++) {
      uint64_t offset =
          target_page(i, total_pages, changed_pages, spread) * page_size;
      transfer(fd, actual, page_size, offset, 0);
      for (size_t j = 0; j < page_size; j++) actual[j] ^= 0xff;
      transfer(fd, actual, page_size, offset, 1);
    }
    // Host filestore synchronization is deliberately controlled by the harness.
    return close(fd) != 0;
  }
  uint64_t state = 0x9e3779b97f4a7c15ULL;
  uint64_t next_changed = 0;
  for (uint64_t off = 0; off < size;) {
    size_t n = size - off < sizeof(words) ? size - off : sizeof(words);
    for (size_t i = 0; i < n / sizeof(uint64_t); i++) {
      state ^= state << 13;
      state ^= state >> 7;
      state ^= state << 17;
      words[i] = state;
    }
    unsigned char* expected = (unsigned char*)words;
    if (verify) {
      for (size_t pos = 0; pos < n; pos += page_size) {
        if (next_changed < changed_pages &&
            off / page_size + pos / page_size ==
                target_page(next_changed, total_pages, changed_pages, spread)) {
          for (size_t j = 0; j < page_size; j++) expected[pos + j] ^= 0xff;
          next_changed++;
        }
      }
      transfer(fd, actual, n, off, 0);
      if (memcmp(expected, actual, n)) {
        fprintf(stderr, "payload differs at offset %llu\n",
                (unsigned long long)off);
        return 1;
      }
    } else {
      transfer(fd, expected, n, off, 1);
    }
    off += n;
  }
  struct stat info;
  if (fstat(fd, &info)) fail("fstat");
  if ((uint64_t)info.st_size != size) return 1;
  if (!verify && fsync(fd)) fail("fsync");
  if (close(fd)) fail("close");
  if (verify || write_only) return 0;
  fd = open("/ready", O_WRONLY | O_CREAT | O_EXCL, 0600);
  if (fd < 0) fail("ready");
  if (close(fd)) fail("close ready");
  for (;;) pause();
}
