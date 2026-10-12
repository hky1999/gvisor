load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "processbirth",
    srcs = ["processbirth.go"],
    visibility = ["//runsc:__subpackages__"],
    deps = ["@org_golang_x_sys//unix:go_default_library"],
)

go_test(
    name = "processbirth_test",
    size = "small",
    srcs = ["processbirth_test.go"],
    library = ":processbirth",
    deps = ["@org_golang_x_sys//unix:go_default_library"],
)
