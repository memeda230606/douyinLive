//go:build p3accacceptance && !windows

package main

func resetP3ACCProcessTreeResourceTracker() {}

func readP3ACCProcessTreeResourceSample() p3ACCProcessTreeResourceSample {
	return p3ACCProcessTreeResourceSample{Complete: false, ProcessCount: 1}
}

func resetP3ACCDataRootPhysicalTracker() {}

func readP3ACCDataRootPhysicalSample(string) (int64, bool) {
	return 0, false
}
