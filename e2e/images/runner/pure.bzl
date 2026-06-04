"""A cfg transition that builds a go_binary in pure (cgo-off, static) mode.

The dep's //cmd/bb_runner isn't built `pure`, so under the musl C toolchain it
links dynamically against /lib/ld-musl-*.so.1, which a scratch image lacks.
Wrapping it with pure_go_binary forces a static, interpreter-free binary without
needing a global --@rules_go//go/config:pure flag.
"""

def _pure_transition_impl(_settings, _attr):
    return {"@rules_go//go/config:pure": True}

_pure_transition = transition(
    implementation = _pure_transition_impl,
    inputs = [],
    outputs = ["@rules_go//go/config:pure"],
)

def _pure_go_binary_impl(ctx):
    # An attribute with an attached transition is a list of configured targets.
    return [DefaultInfo(files = ctx.attr.binary[0][DefaultInfo].files)]

pure_go_binary = rule(
    implementation = _pure_go_binary_impl,
    attrs = {
        "binary": attr.label(
            cfg = _pure_transition,
            mandatory = True,
            doc = "The go_binary to build in pure mode.",
        ),
        "_allowlist_function_transition": attr.label(
            default = "@bazel_tools//tools/allowlists/function_transition_allowlist",
        ),
    },
)
