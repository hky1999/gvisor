load("//tools:defs.bzl", "go_library", "go_test")

package(default_applicable_licenses = ["//:license"])

go_library(
    name = "park",
    srcs = ["park.go"],
    visibility = ["//runsc:__subpackages__"],
    deps = ["@org_golang_x_sys//unix:go_default_library"],
)

go_test(
    name = "park_test",
    size = "small",
    srcs = ["park_test.go"],
    embed = [":park"],
    deps = ["@org_golang_x_sys//unix:go_default_library"],
)
