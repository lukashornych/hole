package engine

import "syscall"

// ioctlGetTermios is the request that reads terminal attributes on this platform.
const ioctlGetTermios = syscall.TCGETS
