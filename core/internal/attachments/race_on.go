//go:build race

package attachments

// raceBuild is true under the race detector, which maps terabytes of
// shadow memory at start: the page counter cannot set RLIMIT_DATA there.
const raceBuild = true
