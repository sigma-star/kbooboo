# Introduction

For someone who runs their own kernels but is not a kernel developer,
finding the right place to report an issue can be challenging.
Because very few kernel developers use Bugzilla, reporting bugs to
bugzilla.kernel.org is often in vain. The vast majority of issues are
reported and handled via email. However, figuring out exactly where
to send that email is half the battle.

While all contacts are documented in the Linux `MAINTAINERS` file, parsing
it manually is cumbersome. As of Linux 7.0-rc3, it contains over 3,000
entries.
Utilities like `scripts/get_maintainer.pl` help locate the right entries,
but they require either a source file or a patch as input.
If all you have is a kernel error (whether it's a panic, BUG, WARN, or Oops),
things become complicated.

This is where this web-based tool comes in. It can look up the correct `MAINTAINERS`
entries directly from a kernel error (a series of log lines containing a stack trace).
It achieves this by consulting a pre-computed lookup table, getting your report to
the right developers quickly and easily.

# Creating the lookup table (`data.json.gz`)

The `datagen` tool operates on a compiled kernel and its source code.
To achieve comprehensive coverage, an `allmodconfig` build is recommended.
It is also necessary to have DWARF debug information enabled, ideally using
`CONFIG_DEBUG_INFO_DWARF5`.

Running datagen multiple times will update the generated `data.json.gz` file.
This is useful for building a single lookup table that covers multiple architectures.
Only one build needs to use `allmodconfig`. For subsequent architectures,
a standard `defconfig` is usually sufficient.

e.g.

    rw@foxxylove:~/linux (master)> ~/datagen --build /scratch2/rw/kbuild
    2026/03/15 23:41:31 Scanning MAINTAINERS
    2026/03/15 23:41:31 Found 3223 MAINTAINERS entries
    2026/03/15 23:41:31 Scanning objects
    2026/03/15 23:42:14 Found 28922 objects
    2026/03/15 23:42:14 Matching sources to objects
    2026/03/15 23:42:25 Object match rate 99.07%
    2026/03/15 23:42:30 Wrote /home/rw/linux/data.json.gz
    rw@foxxylove:~/linux (master)> ~/datagen --build /scratch2/rw/kbuild-arm64
    2026/03/15 23:42:44 Updating /home/rw/linux/data.json.gz
    2026/03/15 23:42:44 Scanning MAINTAINERS
    2026/03/15 23:42:44 Found 3223 MAINTAINERS entries
    2026/03/15 23:42:44 Scanning objects
    2026/03/15 23:42:46 Found 9191 objects
    2026/03/15 23:42:46 Matching sources to objects
    2026/03/15 23:42:50 Object match rate 99.04%
    2026/03/15 23:42:51 Found 12912 new symbols
    2026/03/15 23:42:54 Wrote /home/rw/linux/data.json.gz
