//go:build p5stbacceptance

package capture

// RecorderErrorCodeForAcceptance exposes only the stable allowlisted error
// code to the P5 stability fixture. Raw FFmpeg diagnostics remain private.
func RecorderErrorCodeForAcceptance(stderrSummary string) string {
	return classifyRecorderExit(stderrSummary)
}
