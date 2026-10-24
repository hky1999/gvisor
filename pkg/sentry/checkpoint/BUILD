load("//tools:defs.bzl", "go_library", "go_test")

package(default_applicable_licenses = ["//:license"])

licenses(["notice"])

go_library(
    name = "checkpoint",
    srcs = [
        "checkpoint.go",
        "clone_filestore.go",
        "filestore_snapshot.go",
    ],
    visibility = ["//pkg/sentry:internal"],
    deps = ["@org_golang_x_sys//unix:go_default_library"],
)

go_test(
    name = "clone_filestore_test",
    size = "small",
    srcs = ["clone_filestore_test.go"],
    library = ":checkpoint",
    deps = ["@org_golang_x_sys//unix:go_default_library"],
)
