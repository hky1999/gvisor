#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <inttypes.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

enum { PAGE = 4096, MIB = 1024 * 1024, WARM_PAGES = 1024 };

static void check(int passed, const char *message) {
    if (!passed) {
        perror(message);
        exit(1);
    }
}

static uint64_t now(void) {
    struct timespec t;
    check(clock_gettime(CLOCK_MONOTONIC, &t) == 0, "clock");
    return (uint64_t)t.tv_sec * 1000000000 + t.tv_nsec;
}

static uint64_t step(uint64_t *state) {
    *state ^= *state << 13;
    *state ^= *state >> 7;
    *state ^= *state << 17;
    return *state;
}

static int compare(const void *a, const void *b) {
    uint64_t left = *(const uint64_t *)a, right = *(const uint64_t *)b;
    return (left > right) - (left < right);
}

static void report(uint64_t *samples, unsigned operations, uint64_t elapsed, uint64_t bytes, uint64_t checksum) {
    qsort(samples, operations, sizeof(*samples), compare);
    printf("{\"operations\":%u,\"bytes\":%"PRIu64",\"seconds\":%.9f,"
           "\"p50Us\":%.3f,\"p99Us\":%.3f,\"opsPerSecond\":%.3f,\"checksum\":%"PRIu64"}\n",
           operations, bytes, elapsed / 1e9, samples[(operations + 1) / 2 - 1] / 1e3,
           samples[(operations * 99 + 99) / 100 - 1] / 1e3, operations * 1e9 / elapsed, checksum);
}

enum read_mode { RANDOM_MMAP, SEQUENTIAL_PREAD, SEQUENTIAL_MMAP };

static void cold_read(enum read_mode mode, unsigned operations) {
    int fd = open("/payload", O_RDONLY);
    check(fd >= 0, "open payload");
    struct stat info;
    check(fstat(fd, &info) == 0 && info.st_size >= 2L*MIB, "stat payload");
    int sequential = mode != RANDOM_MMAP;
    size_t chunk = sequential ? 256 * 1024 : PAGE;
    unsigned char *buffer = malloc(chunk);
    uint64_t *samples = malloc(operations * sizeof(*samples));
    check(buffer && samples, "allocate benchmark buffers");
    // The mmap sequence also tests the platform fault path directly.
    check(!sequential || (uint64_t)operations * chunk <= (uint64_t)info.st_size, "sequential range");
    volatile const unsigned char *mapped = mode == SEQUENTIAL_PREAD ? NULL :
        mmap(NULL, info.st_size, PROT_READ, MAP_SHARED, fd, 0);
    check(mapped != MAP_FAILED, "map payload");
    uint64_t checksum = 0, seed = 0xdacf114f0912feabULL;
    uint64_t started = now();
    for (unsigned i = 0; i < operations; ++i) {
        off_t offset = sequential ? (off_t)i * chunk : (off_t)(step(&seed) % (info.st_size / PAGE)) * PAGE;
        uint64_t begin = now();
        if (mode == SEQUENTIAL_MMAP) {
            for (size_t j = 0; j < chunk; j += PAGE)
                checksum = checksum * 13 + mapped[offset + j];
        } else if (sequential) {
            check(pread(fd, buffer, chunk, offset) == chunk, "read sequential");
            checksum = (checksum * 13) + buffer[0];
        } else {
            checksum = (checksum * 13) + mapped[offset] + mapped[offset + PAGE - 1];
        }
        samples[i] = now() - begin;
    }
    report(samples, operations, now() - started, operations * chunk, checksum);
    if (mapped) munmap((void *)mapped, info.st_size);
    free(samples);
    free(buffer);
    check(close(fd) == 0, "close payload");
}

static void make_warm(void) {
    int fd = open("/warm-file", O_CREAT | O_EXCL | O_RDWR, 0600);
    check(fd >= 0, "create warm file");
    unsigned char data[PAGE];
    memset(data, 0x31, sizeof(data));
    for (unsigned i = 0; i < WARM_PAGES; ++i)
        check(write(fd, data, sizeof(data)) == sizeof(data), "seed warm file");
    check(close(fd) == 0, "close warm file");
}

static void warm_write(unsigned operations, int durability) {
    int fd = open("/warm-file", O_RDWR);
    check(fd >= 0, "open warm file");
    unsigned char data[PAGE];
    for (unsigned i = 0; i < WARM_PAGES; ++i)
        check(pread(fd, data, sizeof(data), (off_t)i * PAGE) == sizeof(data), "preload warm file");
    uint64_t *samples = malloc(operations * sizeof(*samples));
    check(samples != NULL, "allocate write timings");
    uint64_t seed = 0x63f333f1f084352aULL, started = now();
    for (unsigned i = 0; i < operations; ++i) {
        off_t offset = (off_t)(step(&seed) % (WARM_PAGES - 2)) * PAGE;
        memset(data, (unsigned char)(i ^ 0x86), sizeof(data));
        memcpy(data, &i, sizeof(i));
        uint64_t begin = now();
        check(pwrite(fd, data, sizeof(data), offset) == sizeof(data), "warm pwrite");
        samples[i] = now() - begin;
        if (durability) {
            printf("%"PRIu64"\n", samples[i]);
            check(fflush(stdout) == 0, "acknowledge pwrite");
            check(getchar() == '\n', "wait for host fsync");
        }
    }
    report(samples, operations, now() - started, (uint64_t)operations * PAGE, 0);
    free(samples);
    check(close(fd) == 0, "close written file");
}

static void shared_mmap(int verify) {
    int fd = open("/warm-file", O_RDWR);
    check(fd >= 0, "open mapped file");
    off_t offset = (WARM_PAGES - 2) * PAGE;
    unsigned char *mapping = mmap(NULL, 2 * PAGE, PROT_READ | PROT_WRITE, MAP_SHARED, fd, offset);
    check(mapping != MAP_FAILED, "map writer");
    if (!verify) {
        check(mapping[9] == 0x31 && mapping[PAGE + 19] == 0x31, "old mapped data");
        mapping[9] = 0xf2;
        pid_t child = fork();
        check(child >= 0, "fork mapper");
        if (child == 0) {
            unsigned char *alias = mmap(NULL, 2 * PAGE, PROT_READ | PROT_WRITE, MAP_SHARED, fd, offset);
            check(alias != MAP_FAILED && alias[9] == 0xf2, "observe first writer from alias");
            alias[PAGE + 19] = 0xab;
            _exit(0);
        }
        int status;
        check(waitpid(child, &status, 0) == child && WIFEXITED(status) && WEXITSTATUS(status) == 0, "mapped child");
    }
    check(mapping[9] == 0xf2 && mapping[PAGE + 19] == 0xab &&
          mapping[10] == 0x31 && mapping[PAGE + 20] == 0x31, "both mapped writes");
    check(munmap(mapping, 2 * PAGE) == 0, "unmap writers");
    check(close(fd) == 0, "close mapped file");
}

// The parent faults both G0 pages through an O_RDONLY descriptor. The
// child opens its own writable descriptor. The parent keeps its mappings
// throughout and reads them again only after waitpid.
static void aliases(int verify) {
    unsigned char page[PAGE];
    int fd = open("/alias-pages", O_RDONLY);
    check(fd >= 0, "open aliases");
    volatile const unsigned char *ro = mmap(NULL, 2*PAGE, PROT_READ, MAP_SHARED, fd, 0);
    check(ro != MAP_FAILED, "map reader");
    if (!verify) {
        check(ro[7]==0x37 && ro[PAGE+7]==0x37, "fault old reader pages");
        pid_t pid=fork();
        check(pid>=0,"fork writer");
        if (!pid) {
            int writer=open("/alias-pages",O_RDWR);
            check(writer>=0,"open writer");
            unsigned char *rw=mmap(NULL,PAGE,PROT_READ|PROT_WRITE,MAP_SHARED,writer,0);
            check(rw!=MAP_FAILED,"map writer");
            rw[7]=0x6b;
            memset(page,0x95,sizeof(page));
            check(pwrite(writer,page,sizeof(page),PAGE)==PAGE,"overwrite inherited page");
            _exit(0);
        }
        int status;
        check(waitpid(pid,&status,0)==pid && WIFEXITED(status) && WEXITSTATUS(status)==0,"finish writer");
    }
    for(unsigned i=0;i<PAGE;i++) {
        check(ro[i]==(i==7?0x6b:0x37),"retained mmap alias");
        check(ro[PAGE+i]==0x95,"retained pwrite alias");
    }
    check(munmap((void*)ro,2*PAGE)==0 && close(fd)==0,"close reader");
    puts("{\"readOnlyAlias\":true,\"mmapByteWrite\":true,\"wholePagePwrite\":true}");
}

static void interior_hole(const char *action) {
    int seed = strcmp(action, "hole-seed") == 0;
    int fd = open("/hole-probe", seed ? O_CREAT | O_EXCL | O_RDWR : O_RDWR, 0600);
    check(fd >= 0, "open hole probe");
    unsigned char pages[3 * PAGE];
    memset(pages, 0x57, sizeof(pages));
    if (seed) {
        check(write(fd, pages, sizeof(pages)) == sizeof(pages), "seed hole probe");
    } else {
        int supported = 1;
        if (strcmp(action, "hole-mutate") == 0 && fallocate(fd, FALLOC_FL_PUNCH_HOLE | FALLOC_FL_KEEP_SIZE, PAGE, PAGE) < 0) {
            check(errno == EOPNOTSUPP || errno == ENOSYS, "guest punch hole");
            supported = 0;
        }
        if (strcmp(action, "hole-unavailable") == 0) supported = 0;
        unsigned char actual[sizeof(pages)];
        if (supported) memset(pages + PAGE, 0, PAGE);
        struct stat st;
        check(fstat(fd, &st) == 0 && st.st_size == sizeof(actual), "hole file size");
        check(pread(fd, actual, sizeof(actual), 0) == sizeof(actual) &&
              memcmp(actual, pages, sizeof(actual)) == 0, "hole and both neighbours");
        printf("{\"guestPunchSupported\":%s}\n", supported ? "true" : "false");
    }
    check(close(fd) == 0, "close hole probe");
}

int main(int argc, char **argv) {
    if (argc < 2) return 2;
    unsigned operations = argc == 3 ? strtoul(argv[2], NULL, 10) : 1;
    if (operations == 0 || operations > 65536) return 2;
    if (strcmp(argv[1], "seed") == 0) {
        make_warm();
        interior_hole("hole-seed");
        int fd=open("/alias-pages",O_CREAT|O_EXCL|O_WRONLY,0600);
        unsigned char buf[2*PAGE];
        memset(buf,0x37,sizeof(buf));
        check(fd>=0 && write(fd,buf,sizeof(buf))==sizeof(buf) && close(fd)==0,"seed aliases");
    }
    else if (strncmp(argv[1], "hole-", 5) == 0) interior_hole(argv[1]);
    else if(strcmp(argv[1],"aliases")==0) aliases(0);
    else if(strcmp(argv[1],"aliases-verify")==0) aliases(1);
    else if (strcmp(argv[1], "seq") == 0) cold_read(SEQUENTIAL_PREAD, operations);
    else if (strcmp(argv[1], "seq-mmap") == 0) cold_read(SEQUENTIAL_MMAP, operations);
    else if (strcmp(argv[1], "random-read") == 0) cold_read(RANDOM_MMAP, operations);
    else if (strcmp(argv[1], "warm-write") == 0) warm_write(operations, 0);
    else if (strcmp(argv[1], "fsync-write") == 0) warm_write(operations, 1);
    else if (strcmp(argv[1], "mmap-mutate") == 0) shared_mmap(0);
    else if (strcmp(argv[1], "mmap-verify") == 0) shared_mmap(1);
    else return 2;
    return 0;
}
