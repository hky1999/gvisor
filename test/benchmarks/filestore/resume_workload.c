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
#include <poll.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <unistd.h>

// Reuse the payload format and commands of the rewrite workload.
#define main rewrite_workload_main
#include "rewrite_workload.c"
#undef main

static const char* payload_path(void) {
  const char* path = getenv("FILESTORE_BENCH_PAYLOAD");
  return path ? path : "/payload";
}

static void postwrite(uint64_t size, uint64_t changed) {
  if (changed > size || changed % 4096) exit(2);
  int fd = open(payload_path(), O_RDWR);
  if (fd < 0) fail("postwrite open");
  struct stat info;
  if (fstat(fd, &info)) fail("postwrite fstat");
  if ((uint64_t)info.st_size != size) exit(2);
  unsigned char data[65536];
  for (uint64_t off = 0; off < changed;) {
    size_t n = changed - off < sizeof(data) ? changed - off : sizeof(data);
    transfer(fd, data, n, off, 0);
    for (size_t i = 0; i < n; i++) data[i] ^= 0x55;
    transfer(fd, data, n, off, 1);
    off += n;
  }
  if (close(fd)) fail("postwrite close");
}

static int verify_post(uint64_t size, uint64_t before, uint64_t after) {
  if (before > size || after > size || size % 4096 || before % 4096 ||
      after % 4096 || size > INT64_MAX)
    return 2;
  int fd = open(payload_path(), O_RDONLY);
  if (fd < 0) fail("verify post open");
  unsigned char actual[65536];
  uint64_t words[65536 / sizeof(uint64_t)];
  uint64_t state = 0x9e3779b97f4a7c15ULL;
  for (uint64_t off = 0; off < size;) {
    size_t n = size - off < sizeof(words) ? size - off : sizeof(words);
    for (size_t i = 0; i < n / sizeof(uint64_t); i++) {
      state ^= state << 13;
      state ^= state >> 7;
      state ^= state << 17;
      words[i] = state;
    }
    unsigned char* expected = (unsigned char*)words;
    if (off < before) {
      size_t count = before - off < n ? before - off : n;
      for (size_t i = 0; i < count; i++) expected[i] ^= 0xff;
    }
    if (off < after) {
      size_t count = after - off < n ? after - off : n;
      for (size_t i = 0; i < count; i++) expected[i] ^= 0x55;
    }
    transfer(fd, actual, n, off, 0);
    if (memcmp(actual, expected, n)) {
      fprintf(stderr, "postwrite payload differs at offset %llu\n",
              (unsigned long long)off);
      close(fd);
      return 1;
    }
    off += n;
  }
  struct stat info;
  if (fstat(fd, &info)) fail("verify post fstat");
  int result = (uint64_t)info.st_size != size;
  if (close(fd)) fail("verify post close");
  return result;
}

int main(int argc, char** argv) {
  if (argc == 4 && !strcmp(argv[1], "postwrite")) {
    postwrite(number(argv[2]), number(argv[3]));
    return 0;
  }
  if (argc == 5 && !strcmp(argv[1], "verify-post"))
    return verify_post(number(argv[2]), number(argv[3]), number(argv[4]));
  if (argc != 4 || strcmp(argv[1], "serve-heartbeat"))
    return rewrite_workload_main(argc, argv);
  uint64_t size = number(argv[2]);
  uint64_t changed = number(argv[3]);
  if (changed > size || changed % 4096) return 2;
  char* populate[] = {argv[0], "write", argv[2]};
  int result = rewrite_workload_main(3, populate);
  if (result) return result;
  const char* ready = getenv("FILESTORE_BENCH_READY");
  if (!ready) ready = "/ready";
  int fd = open(ready, O_WRONLY | O_CREAT | O_EXCL, 0600);
  if (fd < 0) fail("heartbeat ready");
  if (close(fd)) fail("heartbeat ready close");
  uint64_t sequence = 0;
  pid_t writer = -1;
  int writable = 1;
  for (;;) {
    if (dprintf(STDOUT_FILENO, "H %llu\n", (unsigned long long)sequence++) < 0)
      fail("heartbeat write");
    struct pollfd input = {.fd = STDIN_FILENO, .events = POLLIN};
    int count = poll(&input, writable ? 1 : 0, 1);
    if (count < 0 && errno != EINTR) fail("heartbeat poll");
    if (count > 0 && (input.revents & (POLLIN | POLLHUP))) {
      char command;
      ssize_t n = read(STDIN_FILENO, &command, 1);
      if (n == 0) writable = 0;
      if (n < 0 && errno != EINTR) fail("heartbeat command");
      if (n == 1) {
        if (command != 'W' || writer != -1) return 2;
        if (dprintf(STDOUT_FILENO, "R\n") < 0) fail("resume report");
        writer = fork();
        if (writer < 0) fail("postwrite fork");
        if (!writer) {
          close(STDIN_FILENO);
          if (dprintf(STDOUT_FILENO, "B\n") < 0) fail("postwrite report");
          postwrite(size, changed);
          if (dprintf(STDOUT_FILENO, "W %llu\n", (unsigned long long)changed) <
              0)
            fail("postwrite done");
          _exit(0);
        }
      }
    }
    if (writer > 0) {
      int status;
      pid_t waited = waitpid(writer, &status, WNOHANG);
      if (waited < 0 && errno != EINTR) fail("postwrite wait");
      if (waited == writer) {
        if (!WIFEXITED(status) || WEXITSTATUS(status)) return 1;
        writer = -2;
      }
    }
  }
}
