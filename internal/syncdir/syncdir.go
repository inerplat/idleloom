// Package syncdir flushes a directory entry so a rename into it survives a
// crash. Writing a file durably is only half of an atomic replace: the
// contents are on disk but the name pointing at them may not be.
//
// It is one package rather than a helper per caller because the Windows half
// is a no-op, and a caller that grows its own copy gets the Unix behaviour by
// default — which on Windows is not a weaker guarantee but a hard error, on a
// path that is usually only reached when something is already going wrong.
package syncdir
