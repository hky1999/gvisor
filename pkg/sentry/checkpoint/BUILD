load("//tools:defs.bzl", "go_library", "go_test")

package(default_applicable_licenses = ["//:license"])

licenses(["notice"])

go_library(
    name = "checkpoint",
    srcs = [
        "checkpoint.go",
        "filestore_snapshot.go",
        "filestore_transfer.go",
    ],
    importpath = "gvisor.dev/gvisor/pkg/sentry/checkpoint",
    visibility = ["//pkg/sentry:internal"],
    deps = [
        "//pkg/unet",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "checkpoint_test",
    size = "small",
    srcs = [
        "checkpoint_test.go",
        "filestore_transfer_test.go",
    ],
    embed = [":checkpoint"],
    deps = ["//pkg/unet"],
)
