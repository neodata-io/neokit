package disk

// isMeasurable reports whether path names something that actually lives on a
// filesystem, and so has free/total space worth asking the kernel about.
//
// ":memory:" is rejected by name. That string is not a filesystem concept — it
// is how SQLite (and several other embedded stores) spell "this database never
// touches disk". Special-casing another library's vocabulary in a general
// filesystem package is a leak, and it is kept deliberately anyway: callers
// hand this the same path string they hand their store, and on a unix system
// ":memory:" is a perfectly legal relative filename, so filepath.Dir would
// reduce it to "." and Usage would confidently report the *working directory's*
// disk as if it were the database's. Silently answering about the wrong
// filesystem is worse than answering nothing.
func isMeasurable(path string) bool {
	return path != "" && path != ":memory:"
}
