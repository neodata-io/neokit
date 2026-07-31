package app

import "os"

// osGetenv is a seam so a test can drive the "is tracing exporting?" report line
// without setting a process-wide environment variable.
var osGetenv = os.Getenv
