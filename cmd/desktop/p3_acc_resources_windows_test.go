//go:build p3accacceptance && windows

package main

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func p3ACCTestObservation(processID uint32, createdAt uint64) p3ACCProcessObservation {
	return p3ACCProcessObservation{
		identity: p3ACCProcessIdentity{processID: processID, createdAt: createdAt},
		threads:  1, workingSet: 2, private: 3, handles: 4,
	}
}

func p3ACCTestJobAccounting(counter int64, total, active, terminated uint32) p3ACCJobAccounting {
	return p3ACCJobAccounting{
		cpu100NS: counter, pageFaults: counter,
		readOperations: counter, writeOperations: counter, otherOperations: counter,
		readBytes: counter, writeBytes: counter, otherBytes: counter,
		totalProcesses: total, activeProcesses: active, totalTerminatedProcesses: terminated,
	}
}

func TestP3ACCProcessTreeJobAccountingSurvivesMemberChurnInOneSample(t *testing.T) {
	resetP3ACCProcessTreeResourceTracker()
	root := p3ACCTestObservation(100, 1)
	child := p3ACCTestObservation(101, 2)
	first := p3ACCTestJobAccounting(100, 2, 2, 0)
	if sample := p3ACCUpdateProcessTreeResourceTracker(
		[]p3ACCProcessObservation{root, child}, true,
		[]uint32{100, 101}, []uint32{100, 101}, first, first,
	); !sample.Complete || sample.CPU100NS != 100 || sample.ProcessCount != 2 {
		t.Fatalf("initial job sample was not accepted: %#v", sample)
	}

	afterExit := p3ACCTestJobAccounting(110, 2, 1, 0)
	if sample := p3ACCUpdateProcessTreeResourceTracker(
		[]p3ACCProcessObservation{root}, true,
		[]uint32{100}, []uint32{100}, afterExit, afterExit,
	); !sample.Complete || sample.CPU100NS != 110 || sample.ProcessCount != 1 {
		t.Fatalf("a child exit caused more than its racing sample to fail: %#v", sample)
	}

	replacement := p3ACCTestObservation(101, 3)
	afterReplacement := p3ACCTestJobAccounting(120, 3, 2, 0)
	if sample := p3ACCUpdateProcessTreeResourceTracker(
		[]p3ACCProcessObservation{root, replacement}, true,
		[]uint32{100, 101}, []uint32{100, 101}, afterReplacement, afterReplacement,
	); !sample.Complete || sample.CPU100NS != 120 || sample.ProcessCount != 2 {
		t.Fatalf("same-PID replacement identity was not accepted safely: %#v", sample)
	}
}

func TestP3ACCProcessTreeIncompleteAttemptDoesNotDestroyLastGoodAccounting(t *testing.T) {
	resetP3ACCProcessTreeResourceTracker()
	observation := p3ACCTestObservation(100, 1)
	baseline := p3ACCTestJobAccounting(100, 1, 1, 0)
	if sample := p3ACCUpdateProcessTreeResourceTracker(
		[]p3ACCProcessObservation{observation}, true,
		[]uint32{100}, []uint32{100}, baseline, baseline,
	); !sample.Complete {
		t.Fatalf("baseline was not accepted: %#v", sample)
	}
	failed := p3ACCTestJobAccounting(110, 1, 1, 0)
	if sample := p3ACCUpdateProcessTreeResourceTracker(
		[]p3ACCProcessObservation{observation}, false,
		[]uint32{100}, []uint32{100}, failed, failed,
	); sample.Complete {
		t.Fatalf("incomplete attempt was accepted: %#v", sample)
	}
	recovered := p3ACCTestJobAccounting(120, 1, 1, 0)
	if sample := p3ACCUpdateProcessTreeResourceTracker(
		[]p3ACCProcessObservation{observation}, true,
		[]uint32{100}, []uint32{100}, recovered, recovered,
	); !sample.Complete || sample.CPU100NS != 120 {
		t.Fatalf("the next coherent attempt did not recover immediately: %#v", sample)
	}
}

func TestP3ACCProcessTreeWithinSampleFencesFailClosed(t *testing.T) {
	observation := p3ACCTestObservation(100, 1)
	cases := []struct {
		name     string
		complete bool
		first    []uint32
		second   []uint32
		before   p3ACCJobAccounting
		after    p3ACCJobAccounting
	}{
		{name: "attempt-incomplete", complete: false, first: []uint32{100}, second: []uint32{100}, before: p3ACCTestJobAccounting(10, 1, 1, 0), after: p3ACCTestJobAccounting(10, 1, 1, 0)},
		{name: "member-churn", complete: true, first: []uint32{100}, second: []uint32{101}, before: p3ACCTestJobAccounting(10, 1, 1, 0), after: p3ACCTestJobAccounting(10, 1, 1, 0)},
		{name: "total-changed", complete: true, first: []uint32{100}, second: []uint32{100}, before: p3ACCTestJobAccounting(10, 1, 1, 0), after: p3ACCTestJobAccounting(10, 2, 1, 0)},
		{name: "active-changed", complete: true, first: []uint32{100}, second: []uint32{100}, before: p3ACCTestJobAccounting(10, 2, 1, 0), after: p3ACCTestJobAccounting(10, 2, 2, 0)},
		{name: "terminated-changed", complete: true, first: []uint32{100}, second: []uint32{100}, before: p3ACCTestJobAccounting(10, 2, 1, 0), after: p3ACCTestJobAccounting(10, 2, 1, 1)},
		{name: "active-count-mismatch", complete: true, first: []uint32{100}, second: []uint32{100}, before: p3ACCTestJobAccounting(10, 2, 2, 0), after: p3ACCTestJobAccounting(10, 2, 2, 0)},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			resetP3ACCProcessTreeResourceTracker()
			if sample := p3ACCUpdateProcessTreeResourceTracker(
				[]p3ACCProcessObservation{observation}, testCase.complete,
				testCase.first, testCase.second, testCase.before, testCase.after,
			); sample.Complete {
				t.Fatalf("unstable within-sample fence was accepted: %#v", sample)
			}
		})
	}

	counterCases := []struct {
		name   string
		mutate func(*p3ACCJobAccounting)
	}{
		{name: "cpu", mutate: func(value *p3ACCJobAccounting) { value.cpu100NS-- }},
		{name: "page-faults", mutate: func(value *p3ACCJobAccounting) { value.pageFaults-- }},
		{name: "read-operations", mutate: func(value *p3ACCJobAccounting) { value.readOperations-- }},
		{name: "write-operations", mutate: func(value *p3ACCJobAccounting) { value.writeOperations-- }},
		{name: "other-operations", mutate: func(value *p3ACCJobAccounting) { value.otherOperations-- }},
		{name: "read-bytes", mutate: func(value *p3ACCJobAccounting) { value.readBytes-- }},
		{name: "write-bytes", mutate: func(value *p3ACCJobAccounting) { value.writeBytes-- }},
		{name: "other-bytes", mutate: func(value *p3ACCJobAccounting) { value.otherBytes-- }},
	}
	for _, testCase := range counterCases {
		t.Run("counter-"+testCase.name, func(t *testing.T) {
			resetP3ACCProcessTreeResourceTracker()
			before := p3ACCTestJobAccounting(10, 1, 1, 0)
			after := before
			testCase.mutate(&after)
			if sample := p3ACCUpdateProcessTreeResourceTracker(
				[]p3ACCProcessObservation{observation}, true,
				[]uint32{100}, []uint32{100}, before, after,
			); sample.Complete {
				t.Fatalf("regressed within-sample accounting was accepted: %#v", sample)
			}
		})
	}
}

func TestP3ACCProcessTreeCrossSampleCountersAndLifetimeCountsCannotRegress(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*p3ACCJobAccounting)
	}{
		{name: "cpu", mutate: func(value *p3ACCJobAccounting) { value.cpu100NS = 90 }},
		{name: "page-faults", mutate: func(value *p3ACCJobAccounting) { value.pageFaults = 90 }},
		{name: "read-operations", mutate: func(value *p3ACCJobAccounting) { value.readOperations = 90 }},
		{name: "write-operations", mutate: func(value *p3ACCJobAccounting) { value.writeOperations = 90 }},
		{name: "other-operations", mutate: func(value *p3ACCJobAccounting) { value.otherOperations = 90 }},
		{name: "read-bytes", mutate: func(value *p3ACCJobAccounting) { value.readBytes = 90 }},
		{name: "write-bytes", mutate: func(value *p3ACCJobAccounting) { value.writeBytes = 90 }},
		{name: "other-bytes", mutate: func(value *p3ACCJobAccounting) { value.otherBytes = 90 }},
		{name: "total-processes", mutate: func(value *p3ACCJobAccounting) { value.totalProcesses = 4 }},
		{name: "terminated-processes", mutate: func(value *p3ACCJobAccounting) { value.totalTerminatedProcesses = 1 }},
	}
	observation := p3ACCTestObservation(100, 1)
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			resetP3ACCProcessTreeResourceTracker()
			baseline := p3ACCTestJobAccounting(100, 5, 1, 2)
			if sample := p3ACCUpdateProcessTreeResourceTracker(
				[]p3ACCProcessObservation{observation}, true,
				[]uint32{100}, []uint32{100}, baseline, baseline,
			); !sample.Complete {
				t.Fatalf("baseline was not accepted: %#v", sample)
			}
			regressed := p3ACCTestJobAccounting(110, 5, 1, 2)
			testCase.mutate(&regressed)
			if sample := p3ACCUpdateProcessTreeResourceTracker(
				[]p3ACCProcessObservation{observation}, true,
				[]uint32{100}, []uint32{100}, regressed, regressed,
			); sample.Complete {
				t.Fatalf("cross-sample regression was accepted: %#v", sample)
			}
			recovered := p3ACCTestJobAccounting(120, 6, 1, 3)
			if sample := p3ACCUpdateProcessTreeResourceTracker(
				[]p3ACCProcessObservation{observation}, true,
				[]uint32{100}, []uint32{100}, recovered, recovered,
			); !sample.Complete {
				t.Fatalf("a rejected sample corrupted the last good baseline: %#v", sample)
			}
		})
	}
}

func TestP3ACCProcessTreeActiveCountMayChangeAcrossCoherentSamples(t *testing.T) {
	resetP3ACCProcessTreeResourceTracker()
	root := p3ACCTestObservation(100, 1)
	child := p3ACCTestObservation(101, 2)
	baseline := p3ACCTestJobAccounting(100, 5, 2, 1)
	if sample := p3ACCUpdateProcessTreeResourceTracker(
		[]p3ACCProcessObservation{root, child}, true,
		[]uint32{100, 101}, []uint32{100, 101}, baseline, baseline,
	); !sample.Complete {
		t.Fatalf("baseline was not accepted: %#v", sample)
	}
	next := p3ACCTestJobAccounting(110, 5, 1, 1)
	if sample := p3ACCUpdateProcessTreeResourceTracker(
		[]p3ACCProcessObservation{root}, true,
		[]uint32{100}, []uint32{100}, next, next,
	); !sample.Complete {
		t.Fatalf("active process count was incorrectly required to be monotonic: %#v", sample)
	}
}

func TestP3ACCProcessTreeInvalidObservationAndOverflowFailClosed(t *testing.T) {
	valid := p3ACCTestObservation(100, 1)
	cases := []struct {
		name   string
		mutate func(*p3ACCProcessObservation)
	}{
		{name: "zero-pid", mutate: func(value *p3ACCProcessObservation) { value.identity.processID = 0 }},
		{name: "zero-creation", mutate: func(value *p3ACCProcessObservation) { value.identity.createdAt = 0 }},
		{name: "threads", mutate: func(value *p3ACCProcessObservation) { value.threads = -1 }},
		{name: "working-set", mutate: func(value *p3ACCProcessObservation) { value.workingSet = -1 }},
		{name: "private", mutate: func(value *p3ACCProcessObservation) { value.private = -1 }},
		{name: "handles", mutate: func(value *p3ACCProcessObservation) { value.handles = -1 }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			resetP3ACCProcessTreeResourceTracker()
			invalid := valid
			testCase.mutate(&invalid)
			accounting := p3ACCTestJobAccounting(10, 1, 1, 0)
			if sample := p3ACCUpdateProcessTreeResourceTracker(
				[]p3ACCProcessObservation{invalid}, true,
				[]uint32{100}, []uint32{100}, accounting, accounting,
			); sample.Complete {
				t.Fatalf("invalid observation was accepted: %#v", sample)
			}
		})
	}

	resetP3ACCProcessTreeResourceTracker()
	duplicateAccounting := p3ACCTestJobAccounting(10, 2, 2, 0)
	if sample := p3ACCUpdateProcessTreeResourceTracker(
		[]p3ACCProcessObservation{valid, valid}, true,
		[]uint32{100, 101}, []uint32{100, 101}, duplicateAccounting, duplicateAccounting,
	); sample.Complete {
		t.Fatalf("duplicate identity was accepted: %#v", sample)
	}

	resetP3ACCProcessTreeResourceTracker()
	overflow := p3ACCTestObservation(101, 2)
	valid.workingSet = math.MaxInt64
	overflow.workingSet = 1
	if sample := p3ACCUpdateProcessTreeResourceTracker(
		[]p3ACCProcessObservation{valid, overflow}, true,
		[]uint32{100, 101}, []uint32{100, 101}, duplicateAccounting, duplicateAccounting,
	); sample.Complete {
		t.Fatalf("overflowing aggregate was accepted: %#v", sample)
	}
}

func p3ACCTestInstallJobQuery(
	t *testing.T,
	query func(windows.Handle, int32, unsafe.Pointer, uint32, *uint32) error,
) {
	t.Helper()
	original := p3ACCQueryJobInformation
	p3ACCQueryJobInformation = query
	t.Cleanup(func() { p3ACCQueryJobInformation = original })
}

func TestP3ACCCurrentJobAccountingUsesExactABIAndRejectsInvalidPayloads(t *testing.T) {
	valid := p3ACCJobBasicAndIOAccountingInformation{
		BasicInfo: p3ACCJobBasicAccountingInformation{
			TotalUserTime: 10, TotalKernelTime: 20,
			ThisPeriodTotalUserTime: 1, ThisPeriodTotalKernelTime: 2,
			TotalPageFaultCount: 3, TotalProcesses: 4, ActiveProcesses: 2,
			TotalTerminatedProcesses: 1,
		},
		IOInfo: p3ACCJobIOCounters{
			ReadOperationCount: 5, WriteOperationCount: 6, OtherOperationCount: 7,
			ReadTransferCount: 8, WriteTransferCount: 9, OtherTransferCount: 10,
		},
	}
	t.Run("valid", func(t *testing.T) {
		called := false
		p3ACCTestInstallJobQuery(t, func(
			job windows.Handle, class int32, buffer unsafe.Pointer, length uint32, returned *uint32,
		) error {
			called = true
			if job != 0 || class != windows.JobObjectBasicAndIoAccountingInformation ||
				length != uint32(unsafe.Sizeof(valid)) || returned == nil {
				return errors.New("unexpected ABI")
			}
			*(*p3ACCJobBasicAndIOAccountingInformation)(buffer) = valid
			*returned = length
			return nil
		})
		accounting, ok := readP3ACCCurrentJobAccounting()
		if !called || !ok || accounting.cpu100NS != 30 || accounting.pageFaults != 3 ||
			accounting.readOperations != 5 || accounting.writeOperations != 6 ||
			accounting.otherOperations != 7 || accounting.readBytes != 8 ||
			accounting.writeBytes != 9 || accounting.otherBytes != 10 ||
			accounting.totalProcesses != 4 || accounting.activeProcesses != 2 ||
			accounting.totalTerminatedProcesses != 1 {
			t.Fatalf("valid accounting payload was not decoded exactly: %#v, ok=%v", accounting, ok)
		}
	})

	cases := []struct {
		name           string
		mutate         func(*p3ACCJobBasicAndIOAccountingInformation)
		returnedLength int
		queryErr       error
	}{
		{name: "negative-total-user", mutate: func(value *p3ACCJobBasicAndIOAccountingInformation) { value.BasicInfo.TotalUserTime = -1 }},
		{name: "negative-period-user", mutate: func(value *p3ACCJobBasicAndIOAccountingInformation) { value.BasicInfo.ThisPeriodTotalUserTime = -1 }},
		{name: "negative-period-kernel", mutate: func(value *p3ACCJobBasicAndIOAccountingInformation) { value.BasicInfo.ThisPeriodTotalKernelTime = -1 }},
		{name: "cpu-overflow", mutate: func(value *p3ACCJobBasicAndIOAccountingInformation) {
			value.BasicInfo.TotalUserTime = math.MaxInt64
			value.BasicInfo.TotalKernelTime = 1
		}},
		{name: "io-overflow", mutate: func(value *p3ACCJobBasicAndIOAccountingInformation) {
			value.IOInfo.OtherTransferCount = math.MaxInt64 + 1
		}},
		{name: "zero-active", mutate: func(value *p3ACCJobBasicAndIOAccountingInformation) { value.BasicInfo.ActiveProcesses = 0 }},
		{name: "short-return", returnedLength: -1},
		{name: "long-return", returnedLength: 1},
		{name: "native-error", queryErr: windows.ERROR_INVALID_DATA},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			payload := valid
			if testCase.mutate != nil {
				testCase.mutate(&payload)
			}
			p3ACCTestInstallJobQuery(t, func(
				_ windows.Handle, _ int32, buffer unsafe.Pointer, length uint32, returned *uint32,
			) error {
				*(*p3ACCJobBasicAndIOAccountingInformation)(buffer) = payload
				*returned = uint32(int(length) + testCase.returnedLength)
				return testCase.queryErr
			})
			if accounting, ok := readP3ACCCurrentJobAccounting(); ok {
				t.Fatalf("invalid accounting payload was accepted: %#v", accounting)
			}
		})
	}
}

func p3ACCTestWriteJobProcessList(
	buffer unsafe.Pointer,
	bufferLength uint32,
	assigned uint32,
	processIDs []uintptr,
	returnedLength *uint32,
) {
	headerSize := int(unsafe.Sizeof(p3ACCJobProcessIDListHeader{}))
	pointerSize := int(unsafe.Sizeof(uintptr(0)))
	header := (*p3ACCJobProcessIDListHeader)(buffer)
	header.NumberOfAssignedProcesses = assigned
	header.NumberOfProcessIDsInList = uint32(len(processIDs))
	available := (int(bufferLength) - headerSize) / pointerSize
	if available > len(processIDs) {
		available = len(processIDs)
	}
	if available > 0 {
		target := unsafe.Slice((*uintptr)(unsafe.Add(buffer, headerSize)), available)
		copy(target, processIDs[:available])
	}
	*returnedLength = uint32(headerSize + available*pointerSize)
}

func TestP3ACCCurrentJobProcessIDsHandlesCompleteAndPartialResponses(t *testing.T) {
	t.Run("sorts-complete-list", func(t *testing.T) {
		p3ACCTestInstallJobQuery(t, func(
			job windows.Handle, class int32, buffer unsafe.Pointer, length uint32, returned *uint32,
		) error {
			if job != 0 || class != windows.JobObjectBasicProcessIdList {
				return errors.New("unexpected query")
			}
			p3ACCTestWriteJobProcessList(buffer, length, 3, []uintptr{9, 3, 7}, returned)
			return nil
		})
		processIDs, ok := readP3ACCCurrentJobProcessIDs()
		if !ok || len(processIDs) != 3 || processIDs[0] != 3 || processIDs[1] != 7 || processIDs[2] != 9 {
			t.Fatalf("complete process list was not sorted exactly: %#v, ok=%v", processIDs, ok)
		}
	})

	partialCases := []struct {
		name           string
		returnMoreData bool
		lengthMode     string
	}{
		{name: "successful-partial-written", lengthMode: "written"},
		{name: "successful-partial-zero", lengthMode: "zero"},
		{name: "successful-partial-required", lengthMode: "required"},
		{name: "error-more-data-written", returnMoreData: true, lengthMode: "written"},
		{name: "error-more-data-zero", returnMoreData: true, lengthMode: "zero"},
		{name: "error-more-data-required", returnMoreData: true, lengthMode: "required"},
	}
	for _, testCase := range partialCases {
		t.Run(testCase.name, func(t *testing.T) {
			calls := 0
			p3ACCTestInstallJobQuery(t, func(
				_ windows.Handle, _ int32, buffer unsafe.Pointer, length uint32, returned *uint32,
			) error {
				calls++
				headerSize := int(unsafe.Sizeof(p3ACCJobProcessIDListHeader{}))
				pointerSize := int(unsafe.Sizeof(uintptr(0)))
				capacity := (int(length) - headerSize) / pointerSize
				listed := capacity
				if listed > 20 {
					listed = 20
				}
				processIDs := make([]uintptr, listed)
				for index := range processIDs {
					processIDs[index] = uintptr(index + 1)
				}
				p3ACCTestWriteJobProcessList(buffer, length, 20, processIDs, returned)
				if listed < 20 {
					switch testCase.lengthMode {
					case "zero":
						*returned = 0
					case "required":
						*returned = uint32(headerSize + 20*pointerSize)
					}
				}
				if testCase.returnMoreData && listed < 20 {
					return windows.ERROR_MORE_DATA
				}
				return nil
			})
			processIDs, ok := readP3ACCCurrentJobProcessIDs()
			if !ok || calls != 2 || len(processIDs) != 20 || processIDs[0] != 1 || processIDs[19] != 20 {
				t.Fatalf("partial list was not expanded safely: calls=%d ids=%#v ok=%v", calls, processIDs, ok)
			}
		})
	}
}

func TestP3ACCCurrentJobProcessIDsRejectsMalformedPartialLengths(t *testing.T) {
	headerSize := int(unsafe.Sizeof(p3ACCJobProcessIDListHeader{}))
	pointerSize := int(unsafe.Sizeof(uintptr(0)))
	minimumWritten := uint32(headerSize + p3ACCJobProcessListInitialCapacity*pointerSize)
	required := uint32(headerSize + 20*pointerSize)
	invalidLengths := []struct {
		name  string
		value uint32
	}{
		{name: "below-header", value: 1},
		{name: "below-written", value: minimumWritten - 1},
		{name: "above-buffer-below-required", value: minimumWritten + 1},
		{name: "below-required", value: required - 1},
		{name: "above-required", value: required + 1},
		{name: "maximum", value: math.MaxUint32},
	}
	for _, testCase := range invalidLengths {
		t.Run(testCase.name, func(t *testing.T) {
			calls := 0
			p3ACCTestInstallJobQuery(t, func(
				_ windows.Handle, _ int32, buffer unsafe.Pointer, length uint32, returned *uint32,
			) error {
				calls++
				processIDs := make([]uintptr, p3ACCJobProcessListInitialCapacity)
				for index := range processIDs {
					processIDs[index] = uintptr(index + 1)
				}
				p3ACCTestWriteJobProcessList(buffer, length, 20, processIDs, returned)
				*returned = testCase.value
				return windows.ERROR_MORE_DATA
			})
			if processIDs, ok := readP3ACCCurrentJobProcessIDs(); ok || calls != 1 {
				t.Fatalf("malformed partial length was accepted or retried: calls=%d ids=%#v ok=%v", calls, processIDs, ok)
			}
		})
	}
}

func TestP3ACCCurrentJobProcessIDsRejectsMalformedResponses(t *testing.T) {
	zeroReturnedLength := uint32(0)
	cases := []struct {
		name     string
		assigned uint32
		ids      []uintptr
		adjust   int
		err      error
		returned *uint32
	}{
		{name: "empty", assigned: 0},
		{name: "zero-id", assigned: 1, ids: []uintptr{0}},
		{name: "duplicate-id", assigned: 2, ids: []uintptr{7, 7}},
		{name: "oversized-id", assigned: 1, ids: []uintptr{uintptr(math.MaxUint32) + 1}},
		{name: "over-capacity", assigned: p3ACCJobProcessListMaximum + 1},
		{name: "short-return", assigned: 1, ids: []uintptr{7}, adjust: -1},
		{name: "long-return", assigned: 1, ids: []uintptr{7}, adjust: 1 << 20},
		{name: "unexpected-error", assigned: 1, ids: []uintptr{7}, err: windows.ERROR_INVALID_DATA},
		{name: "complete-zero-return", assigned: 1, ids: []uintptr{7}, returned: &zeroReturnedLength},
		{name: "complete-more-data", assigned: 1, ids: []uintptr{7}, err: windows.ERROR_MORE_DATA},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			p3ACCTestInstallJobQuery(t, func(
				_ windows.Handle, _ int32, buffer unsafe.Pointer, length uint32, returned *uint32,
			) error {
				p3ACCTestWriteJobProcessList(buffer, length, testCase.assigned, testCase.ids, returned)
				*returned = uint32(int(*returned) + testCase.adjust)
				if testCase.returned != nil {
					*returned = *testCase.returned
				}
				return testCase.err
			})
			if processIDs, ok := readP3ACCCurrentJobProcessIDs(); ok {
				t.Fatalf("malformed process list was accepted: %#v", processIDs)
			}
		})
	}
}

const (
	p3ACCTestJobHelperModeEnvironment       = "P3ACC_TEST_JOB_HELPER_MODE"
	p3ACCTestJobHelperResultEnvironment     = "P3ACC_TEST_JOB_HELPER_RESULT"
	p3ACCTestJobHelperGoEnvironment         = "P3ACC_TEST_JOB_HELPER_GO"
	p3ACCTestJobHelperWorkEnvironment       = "P3ACC_TEST_JOB_HELPER_WORK"
	p3ACCTestJobHelperReadyEnvironment      = "P3ACC_TEST_JOB_HELPER_READY"
	p3ACCTestJobHelperOuterEnvironment      = "P3ACC_TEST_JOB_HELPER_OUTER"
	p3ACCTestJobHelperControllerEnvironment = "P3ACC_TEST_JOB_HELPER_CONTROLLER"
)

var p3ACCTestWorkloadSink uint64

func p3ACCTestCreateKillOnCloseJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("create job: %w", err)
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	set, setErr := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)),
		uint32(unsafe.Sizeof(limits)),
	)
	if setErr != nil || set == 0 {
		_ = windows.CloseHandle(job)
		return 0, fmt.Errorf("set job limits: result=%d err=%w", set, setErr)
	}
	return job, nil
}

func p3ACCTestCreateSuspendedInJob(job windows.Handle, arguments []string, breakaway bool) (windows.ProcessInformation, error) {
	if job == 0 || len(arguments) == 0 || arguments[0] == "" {
		return windows.ProcessInformation{}, errors.New("invalid launch arguments")
	}
	application, err := windows.UTF16PtrFromString(arguments[0])
	if err != nil {
		return windows.ProcessInformation{}, fmt.Errorf("encode application: %w", err)
	}
	commandLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(arguments))
	if err != nil {
		return windows.ProcessInformation{}, fmt.Errorf("encode command line: %w", err)
	}
	startup := windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfo{}))}
	process := windows.ProcessInformation{}
	creationFlags := uint32(windows.CREATE_SUSPENDED | windows.CREATE_NO_WINDOW)
	if breakaway {
		creationFlags |= windows.CREATE_BREAKAWAY_FROM_JOB
	}
	if err := windows.CreateProcess(
		application,
		commandLine,
		nil,
		nil,
		false,
		creationFlags,
		nil,
		nil,
		&startup,
		&process,
	); err != nil {
		return windows.ProcessInformation{}, fmt.Errorf("create suspended process: %w", err)
	}
	fail := func(cause error) (windows.ProcessInformation, error) {
		_ = windows.TerminateProcess(process.Process, 1)
		_, _ = windows.WaitForSingleObject(process.Process, 5_000)
		_ = windows.CloseHandle(process.Thread)
		_ = windows.CloseHandle(process.Process)
		return windows.ProcessInformation{}, cause
	}
	if err := windows.AssignProcessToJobObject(job, process.Process); err != nil {
		return fail(fmt.Errorf("assign process to job: %w", err))
	}
	return process, nil
}

func p3ACCTestLaunchInJob(job windows.Handle, arguments []string, breakaway bool) (windows.ProcessInformation, error) {
	process, err := p3ACCTestCreateSuspendedInJob(job, arguments, breakaway)
	if err != nil {
		return windows.ProcessInformation{}, err
	}
	fail := func(cause error) (windows.ProcessInformation, error) {
		_ = windows.TerminateProcess(process.Process, 1)
		_, _ = windows.WaitForSingleObject(process.Process, 5_000)
		p3ACCTestCloseProcessInformation(&process)
		return windows.ProcessInformation{}, cause
	}
	previousSuspendCount, err := windows.ResumeThread(process.Thread)
	if err != nil || previousSuspendCount == 0 || previousSuspendCount == math.MaxUint32 {
		return fail(fmt.Errorf("resume process: previous=%d err=%w", previousSuspendCount, err))
	}
	if err := windows.CloseHandle(process.Thread); err != nil {
		return fail(fmt.Errorf("close resumed thread handle: %w", err))
	}
	process.Thread = 0
	return process, nil
}

func p3ACCTestCloseProcessInformation(process *windows.ProcessInformation) {
	if process == nil {

		return
	}
	if process.Thread != 0 {
		_ = windows.CloseHandle(process.Thread)
		process.Thread = 0
	}
	if process.Process != 0 {
		_ = windows.CloseHandle(process.Process)
		process.Process = 0
	}
}

func p3ACCTestWaitDirectJobDrained(job windows.Handle, timeout time.Duration) error {
	if job == 0 || timeout <= 0 {
		return errors.New("invalid direct job drain wait")
	}
	deadline := time.Now().Add(timeout)
	var lastObservation string
	for {
		accounting := p3ACCJobBasicAndIOAccountingInformation{}
		var accountingLength uint32
		accountingSize := uint32(unsafe.Sizeof(accounting))
		accountingErr := p3ACCQueryJobInformation(
			job, windows.JobObjectBasicAndIoAccountingInformation,
			unsafe.Pointer(&accounting), accountingSize, &accountingLength,
		)

		processListBuffer := make([]uintptr, 2)
		var processListLength uint32
		processListSize := uint32(len(processListBuffer)) * uint32(unsafe.Sizeof(processListBuffer[0]))
		processListErr := p3ACCQueryJobInformation(
			job, windows.JobObjectBasicProcessIdList,
			unsafe.Pointer(&processListBuffer[0]), processListSize, &processListLength,
		)
		header := (*p3ACCJobProcessIDListHeader)(unsafe.Pointer(&processListBuffer[0]))
		minimumProcessListLength := uint32(unsafe.Sizeof(p3ACCJobProcessIDListHeader{}))
		accountingValid := accountingErr == nil && accountingLength == accountingSize
		processListValid := processListErr == nil &&
			processListLength >= minimumProcessListLength && processListLength <= processListSize
		if accountingValid && processListValid &&
			accounting.BasicInfo.ActiveProcesses == 0 &&
			header.NumberOfAssignedProcesses == 0 && header.NumberOfProcessIDsInList == 0 {
			return nil
		}
		lastObservation = fmt.Sprintf(
			"accounting active=%d length=%d/%d err=%v; process list assigned=%d listed=%d length=%d/%d err=%v",
			accounting.BasicInfo.ActiveProcesses, accountingLength, accountingSize, accountingErr,
			header.NumberOfAssignedProcesses, header.NumberOfProcessIDsInList,
			processListLength, processListSize, processListErr,
		)
		if !time.Now().Before(deadline) {
			return fmt.Errorf("job did not drain before timeout: %s", lastObservation)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func p3ACCTestStrictlyDrainJob(
	job *windows.Handle,
	processes ...*windows.ProcessInformation,
) error {
	if job == nil {
		return errors.New("nil job handle pointer")
	}
	cleanupErrors := make([]error, 0, len(processes)*2+3)
	record := func(err error) {
		if err != nil {
			cleanupErrors = append(cleanupErrors, err)
		}
	}
	for index, process := range processes {
		if process == nil {
			record(fmt.Errorf("process information %d is nil", index))
			continue
		}
		if process.Thread != 0 {
			handle := process.Thread
			process.Thread = 0
			if err := windows.CloseHandle(handle); err != nil {
				record(fmt.Errorf("close process %d thread handle: %w", index, err))
			}
		}
		if process.Process != 0 {
			handle := process.Process
			process.Process = 0
			if err := windows.CloseHandle(handle); err != nil {
				record(fmt.Errorf("close process %d process handle: %w", index, err))
			}
		}
	}
	if *job == 0 {
		return errors.Join(cleanupErrors...)
	}
	jobHandle := *job
	if err := windows.TerminateJobObject(jobHandle, 1); err != nil {
		record(fmt.Errorf("terminate job: %w", err))
	}
	record(p3ACCTestWaitDirectJobDrained(jobHandle, 5*time.Second))
	*job = 0
	if err := windows.CloseHandle(jobHandle); err != nil {
		record(fmt.Errorf("close job handle: %w", err))
	}
	return errors.Join(cleanupErrors...)
}

func TestP3ACCStrictJobCleanupDrainsSuspendedProcessAndIsIdempotent(t *testing.T) {
	job, err := p3ACCTestCreateKillOnCloseJob()
	if err != nil {
		t.Fatal(err)
	}
	process := windows.ProcessInformation{}
	defer func() {
		if cleanupErr := p3ACCTestStrictlyDrainJob(&job, &process); cleanupErr != nil {
			t.Errorf("deferred strict job cleanup failed: %v", cleanupErr)
		}
	}()
	commandInterpreter := os.Getenv("ComSpec")
	info, err := os.Stat(commandInterpreter)
	if err != nil || !filepath.IsAbs(commandInterpreter) || !info.Mode().IsRegular() {
		t.Fatalf("invalid command interpreter: path=%q err=%v", commandInterpreter, err)
	}
	process, err = p3ACCTestCreateSuspendedInJob(
		job, []string{commandInterpreter, "/d", "/c", "exit", "0"}, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if process.Process == 0 || process.Thread == 0 {
		t.Fatal("suspended process did not retain both owned handles")
	}
	if err := p3ACCTestStrictlyDrainJob(&job, &process); err != nil {
		t.Fatal(err)
	}
	if job != 0 || process.Process != 0 || process.Thread != 0 {
		t.Fatalf("strict cleanup did not zero every handle: job=%d process=%d thread=%d", job, process.Process, process.Thread)
	}
	if err := p3ACCTestStrictlyDrainJob(&job, &process); err != nil {
		t.Fatalf("strict cleanup was not idempotent: %v", err)
	}
}

func p3ACCTestWaitProcess(process windows.Handle, timeout time.Duration) (uint32, error) {
	if process == 0 || timeout <= 0 || timeout > time.Duration(math.MaxUint32)*time.Millisecond {
		return 0, errors.New("invalid process wait")
	}
	waitResult, err := windows.WaitForSingleObject(process, uint32(timeout/time.Millisecond))
	if err != nil || waitResult != uint32(windows.WAIT_OBJECT_0) {
		return 0, fmt.Errorf("wait process: result=%d err=%w", waitResult, err)
	}
	var exitCode uint32
	if err := windows.GetExitCodeProcess(process, &exitCode); err != nil {
		return 0, fmt.Errorf("get exit code: %w", err)
	}
	return exitCode, nil
}

func p3ACCTestProcessIDsContain(processIDs []uint32, expected uint32) bool {
	for _, processID := range processIDs {
		if processID == expected {
			return true
		}
	}
	return false
}

func p3ACCTestProcessIDsStrictlyInclude(processIDs, expected []uint32) bool {
	if len(processIDs) <= len(expected) {
		return false
	}
	for _, expectedProcessID := range expected {
		if !p3ACCTestProcessIDsContain(processIDs, expectedProcessID) {
			return false
		}
	}
	return true
}

func p3ACCTestExerciseRealJobProcessListExpansion(
	baselineMembers []uint32,
	baselineAccounting p3ACCJobAccounting,
) (returnErr error) {
	innerJob, err := p3ACCTestCreateKillOnCloseJob()
	if err != nil {
		return err
	}
	processes := make([]windows.ProcessInformation, 0, 20)
	cleaned := false
	cleanup := func() error {
		processPointers := make([]*windows.ProcessInformation, 0, len(processes))
		for index := range processes {
			processPointers = append(processPointers, &processes[index])
		}
		return p3ACCTestStrictlyDrainJob(&innerJob, processPointers...)
	}
	defer func() {
		if !cleaned {
			if cleanupErr := cleanup(); cleanupErr != nil {
				if returnErr == nil {
					returnErr = cleanupErr
				} else {
					returnErr = fmt.Errorf("%v; cleanup failed: %w", returnErr, cleanupErr)
				}
			}
		}
	}()

	commandInterpreter := os.Getenv("ComSpec")
	info, statErr := os.Stat(commandInterpreter)
	if statErr != nil {
		return fmt.Errorf("invalid command interpreter: %w", statErr)
	}
	if !filepath.IsAbs(commandInterpreter) || !info.Mode().IsRegular() {
		return errors.New("invalid command interpreter")
	}
	for index := 0; index < cap(processes); index++ {
		process, createErr := p3ACCTestCreateSuspendedInJob(
			innerJob, []string{commandInterpreter, "/d", "/c", "exit", "0"}, false,
		)
		if createErr != nil {
			return fmt.Errorf("create suspended fanout process %d: %w", index, createErr)
		}
		processes = append(processes, process)
	}
	originalQuery := p3ACCQueryJobInformation
	queryCalls := 0
	sawPartial := false
	var firstAssigned uint32
	var firstListed uint32
	var firstReturned uint32
	var firstBufferLength uint32
	firstMoreData := false
	p3ACCQueryJobInformation = func(
		job windows.Handle, class int32, information unsafe.Pointer,
		informationLength uint32, returnedLength *uint32,
	) error {
		queryErr := originalQuery(job, class, information, informationLength, returnedLength)
		if class == windows.JobObjectBasicProcessIdList {
			queryCalls++
			if information != nil && informationLength >= uint32(unsafe.Sizeof(p3ACCJobProcessIDListHeader{})) {
				header := (*p3ACCJobProcessIDListHeader)(information)
				if queryCalls == 1 {
					firstAssigned = header.NumberOfAssignedProcesses
					firstListed = header.NumberOfProcessIDsInList
					firstBufferLength = informationLength
					firstMoreData = errors.Is(queryErr, windows.ERROR_MORE_DATA)
					firstReturned = *returnedLength
				}
				if header.NumberOfProcessIDsInList < header.NumberOfAssignedProcesses &&
					(queryErr == nil || errors.Is(queryErr, windows.ERROR_MORE_DATA)) {
					sawPartial = true
				}
			}
		}
		return queryErr
	}
	members, membersOK := readP3ACCCurrentJobProcessIDs()
	p3ACCQueryJobInformation = originalQuery
	if !membersOK || queryCalls < 2 || !sawPartial ||
		!p3ACCTestProcessIDsStrictlyInclude(members, baselineMembers) {
		return fmt.Errorf(
			"real job list did not expand: ok=%t calls=%d partial=%t members=%d baseline=%d assigned=%d listed=%d returned=%d buffer=%d more=%t",
			membersOK, queryCalls, sawPartial, len(members), len(baselineMembers),
			firstAssigned, firstListed, firstReturned, firstBufferLength, firstMoreData,
		)
	}
	for _, process := range processes {
		if !p3ACCTestProcessIDsContain(members, process.ProcessId) {
			return errors.New("real expanded job list omitted a suspended member")
		}
	}
	accounting, accountingOK := readP3ACCCurrentJobAccounting()
	if !accountingOK || int(accounting.activeProcesses) != len(members) ||
		accounting.totalProcesses < baselineAccounting.totalProcesses+uint32(len(processes)) {
		return fmt.Errorf(
			"real expanded job accounting mismatch: ok=%t active=%d members=%d total=%d",
			accountingOK, accounting.activeProcesses, len(members), accounting.totalProcesses,
		)
	}
	if cleanupErr := cleanup(); cleanupErr != nil {
		cleaned = true
		return cleanupErr
	}
	cleaned = true
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		currentMembers, currentMembersOK := readP3ACCCurrentJobProcessIDs()
		currentAccounting, currentAccountingOK := readP3ACCCurrentJobAccounting()
		if currentMembersOK && currentAccountingOK &&
			sameP3ACCJobProcessIDs(baselineMembers, currentMembers) &&
			int(currentAccounting.activeProcesses) == len(baselineMembers) {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("outer job did not return to the exact baseline after fanout cleanup")
}

func TestP3ACCJobAccountingAggregatesNestedExitedChild(t *testing.T) {
	if os.Getenv(p3ACCTestJobHelperModeEnvironment) != "" {
		t.Skip("controller-only test")
	}
	outerJob, err := p3ACCTestCreateKillOnCloseJob()
	if err != nil {
		t.Fatal(err)
	}
	process := windows.ProcessInformation{}
	cleanup := func() error {
		return p3ACCTestStrictlyDrainJob(&outerJob, &process)
	}
	defer func() {
		if cleanupErr := cleanup(); cleanupErr != nil {
			t.Errorf("strict outer job cleanup failed: %v", cleanupErr)
		}
	}()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	resultPath := filepath.Join(t.TempDir(), "nested-job-result.txt")
	t.Setenv(p3ACCTestJobHelperModeEnvironment, "outer")
	t.Setenv(p3ACCTestJobHelperResultEnvironment, resultPath)
	t.Setenv(p3ACCTestJobHelperControllerEnvironment, fmt.Sprintf("%d", os.Getpid()))
	process, err = p3ACCTestLaunchInJob(outerJob, []string{
		executable, "-test.run=^TestP3ACCJobAccountingHelperProcess$", "-test.count=1",
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	exitCode, waitErr := p3ACCTestWaitProcess(process.Process, 30*time.Second)
	if waitErr != nil || exitCode != 0 {
		result, _ := os.ReadFile(resultPath)
		t.Fatalf("nested job helper failed: exit=%d err=%v result=%q", exitCode, waitErr, result)
	}
	result, err := os.ReadFile(resultPath)
	if err != nil || string(result) != "OK\n" {
		t.Fatalf("nested job helper result invalid: result=%q err=%v", result, err)
	}
	if cleanupErr := cleanup(); cleanupErr != nil {
		t.Fatalf("strict outer job cleanup failed: %v", cleanupErr)
	}
}

func p3ACCTestJobHelperFatal(t *testing.T, resultPath, message string, err error) {
	t.Helper()
	payload := "FAIL: " + message
	if err != nil {
		payload += ": " + err.Error()
	}
	_ = os.WriteFile(resultPath, []byte(payload+"\n"), 0o600)
	t.Fatal(payload)
}

func p3ACCTestJobHelperCleanupFailure(t *testing.T, resultPath, message string, err error) {
	t.Helper()
	prior, readErr := os.ReadFile(resultPath)
	payload := "FAIL: " + message + ": " + err.Error()
	if readErr == nil && len(prior) > 0 {
		payload += fmt.Sprintf("; prior=%q", prior)
	} else if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		payload += ": read prior result: " + readErr.Error()
	}
	if writeErr := os.WriteFile(resultPath, []byte(payload+"\n"), 0o600); writeErr != nil {
		t.Errorf("%s; write cleanup failure result: %v", payload, writeErr)
		return
	}
	t.Error(payload)
}

func TestP3ACCJobAccountingHelperProcess(t *testing.T) {
	if os.Getenv(p3ACCTestJobHelperModeEnvironment) != "outer" {
		t.Skip("outer-job helper only")
	}
	resultPath := os.Getenv(p3ACCTestJobHelperResultEnvironment)
	if !filepath.IsAbs(resultPath) {
		p3ACCTestJobHelperFatal(t, resultPath, "helper result path is not absolute", nil)
	}
	beforeMembers, membersOK := readP3ACCCurrentJobProcessIDs()
	before, accountingOK := readP3ACCCurrentJobAccounting()
	currentProcessID := uint32(os.Getpid())
	if !membersOK || !accountingOK || !p3ACCTestProcessIDsContain(beforeMembers, currentProcessID) ||
		int(before.activeProcesses) != len(beforeMembers) {
		currentPresent := false
		controllerPresent := false
		controllerValue := os.Getenv(p3ACCTestJobHelperControllerEnvironment)
		for _, processID := range beforeMembers {
			currentPresent = currentPresent || processID == currentProcessID
			controllerPresent = controllerPresent || fmt.Sprintf("%d", processID) == controllerValue
		}
		memberNames := make([]string, 0, len(beforeMembers))
		if entries, entriesOK := p3ACCCaptureProcessEntries(); entriesOK {
			for _, processID := range beforeMembers {
				if entry, exists := entries[processID]; exists {
					memberNames = append(memberNames, windows.UTF16ToString(entry.ExeFile[:]))
				}
			}
		}
		p3ACCTestJobHelperFatal(t, resultPath, fmt.Sprintf(
			"outer job mismatch membersOK=%t count=%d names=%v currentPresent=%t controllerPresent=%t accountingOK=%t total=%d active=%d",
			membersOK, len(beforeMembers), memberNames, currentPresent, controllerPresent, accountingOK,
			before.totalProcesses, before.activeProcesses,
		), nil)
	}

	innerJob, err := p3ACCTestCreateKillOnCloseJob()
	if err != nil {
		p3ACCTestJobHelperFatal(t, resultPath, "create inner job", err)
	}
	child := windows.ProcessInformation{}
	cleanup := func() error {
		return p3ACCTestStrictlyDrainJob(&innerJob, &child)
	}
	defer func() {
		if cleanupErr := cleanup(); cleanupErr != nil {
			p3ACCTestJobHelperCleanupFailure(t, resultPath, "strict inner job cleanup failed", cleanupErr)
		}
	}()
	executable, err := os.Executable()
	if err != nil {
		p3ACCTestJobHelperFatal(t, resultPath, "resolve helper executable", err)
	}
	goPath := resultPath + ".go"
	workPath := resultPath + ".work"
	readyPath := resultPath + ".ready"
	if err := os.Setenv(p3ACCTestJobHelperModeEnvironment, "workload"); err != nil {
		p3ACCTestJobHelperFatal(t, resultPath, "set workload mode", err)
	}
	if err := os.Setenv(p3ACCTestJobHelperGoEnvironment, goPath); err != nil {
		p3ACCTestJobHelperFatal(t, resultPath, "set workload trigger", err)
	}
	if err := os.Setenv(p3ACCTestJobHelperWorkEnvironment, workPath); err != nil {
		p3ACCTestJobHelperFatal(t, resultPath, "set workload path", err)
	}
	if err := os.Setenv(p3ACCTestJobHelperReadyEnvironment, readyPath); err != nil {
		p3ACCTestJobHelperFatal(t, resultPath, "set workload ready path", err)
	}
	if err := os.Setenv(p3ACCTestJobHelperOuterEnvironment, fmt.Sprintf("%d", currentProcessID)); err != nil {
		p3ACCTestJobHelperFatal(t, resultPath, "set outer helper identity", err)
	}
	child, err = p3ACCTestLaunchInJob(innerJob, []string{
		executable, "-test.run=^TestP3ACCJobAccountingWorkloadProcess$", "-test.count=1",
	}, false)
	if err != nil {
		p3ACCTestJobHelperFatal(t, resultPath, "launch nested workload", err)
	}

	readyDeadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(readyPath); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			p3ACCTestJobHelperFatal(t, resultPath, "read workload ready marker", err)
		}
		waitResult, waitErr := windows.WaitForSingleObject(child.Process, 0)
		if waitErr != nil {
			p3ACCTestJobHelperFatal(t, resultPath, "probe workload before ready", waitErr)
		}
		if waitResult == uint32(windows.WAIT_OBJECT_0) {
			workloadResult, _ := os.ReadFile(resultPath)
			p3ACCTestJobHelperFatal(t, resultPath, "workload exited before ready: "+string(workloadResult), nil)
		}
		if time.Now().After(readyDeadline) {
			p3ACCTestJobHelperFatal(t, resultPath, "workload ready marker timed out", nil)
		}
		time.Sleep(10 * time.Millisecond)
	}

	activeMembers, activeMembersOK := readP3ACCCurrentJobProcessIDs()
	activeBaseline, activeAccountingOK := readP3ACCCurrentJobAccounting()
	if !activeMembersOK || !activeAccountingOK ||
		!p3ACCTestProcessIDsStrictlyInclude(activeMembers, beforeMembers) ||
		!p3ACCTestProcessIDsContain(activeMembers, child.ProcessId) ||
		int(activeBaseline.activeProcesses) != len(activeMembers) ||
		activeBaseline.totalProcesses < before.totalProcesses+1 {
		p3ACCTestJobHelperFatal(t, resultPath, "nested child was not aggregated into outer job", nil)
	}
	resetP3ACCProcessTreeResourceTracker()
	if sample := readP3ACCProcessTreeResourceSample(); !sample.Complete ||
		sample.ProcessCount != int64(len(activeMembers)) {
		p3ACCTestJobHelperFatal(t, resultPath, fmt.Sprintf(
			"full sampler failed complete=%t sampled=%d active=%d",
			sample.Complete, sample.ProcessCount, len(activeMembers),
		), nil)
	}
	if err := os.WriteFile(goPath, []byte("GO\n"), 0o600); err != nil {
		p3ACCTestJobHelperFatal(t, resultPath, "release workload trigger", err)
	}
	exitCode, err := p3ACCTestWaitProcess(child.Process, 20*time.Second)
	if err != nil || exitCode != 0 {
		p3ACCTestJobHelperFatal(t, resultPath, fmt.Sprintf("workload child failed with exit %d", exitCode), err)
	}
	var afterMembers []uint32
	var after p3ACCJobAccounting
	normalized := false
	evidenceReady := false
	deadline := time.Now().Add(5 * time.Second)
	for !evidenceReady && time.Now().Before(deadline) {
		var afterMembersOK, afterAccountingOK bool
		afterMembers, afterMembersOK = readP3ACCCurrentJobProcessIDs()
		after, afterAccountingOK = readP3ACCCurrentJobAccounting()
		normalized = afterMembersOK && afterAccountingOK &&
			sameP3ACCJobProcessIDs(beforeMembers, afterMembers) &&
			int(after.activeProcesses) == len(beforeMembers)
		evidenceReady = normalized && after.totalProcesses >= before.totalProcesses+1 &&
			after.totalTerminatedProcesses >= activeBaseline.totalTerminatedProcesses &&
			p3ACCJobAccountingAtLeast(after, activeBaseline) &&
			after.cpu100NS > activeBaseline.cpu100NS &&
			after.pageFaults > activeBaseline.pageFaults &&
			after.readOperations > activeBaseline.readOperations &&
			after.writeOperations > activeBaseline.writeOperations &&
			after.readBytes > activeBaseline.readBytes &&
			after.writeBytes > activeBaseline.writeBytes
		if !evidenceReady {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !evidenceReady {
		p3ACCTestJobHelperFatal(t, resultPath, fmt.Sprintf(
			"exited accounting mismatch normalized=%t totalDelta=%d cpuDelta=%d pageDelta=%d readOpsDelta=%d writeOpsDelta=%d readBytesDelta=%d writeBytesDelta=%d",
			normalized, int64(after.totalProcesses)-int64(before.totalProcesses),
			after.cpu100NS-activeBaseline.cpu100NS, after.pageFaults-activeBaseline.pageFaults,
			after.readOperations-activeBaseline.readOperations, after.writeOperations-activeBaseline.writeOperations,
			after.readBytes-activeBaseline.readBytes, after.writeBytes-activeBaseline.writeBytes,
		), nil)
	}
	if cleanupErr := cleanup(); cleanupErr != nil {
		p3ACCTestJobHelperFatal(t, resultPath, "strict inner job cleanup", cleanupErr)
	}
	if err := os.Remove(readyPath); err != nil {
		p3ACCTestJobHelperFatal(t, resultPath, "remove workload ready marker", err)
	}
	if err := os.Remove(goPath); err != nil {
		p3ACCTestJobHelperFatal(t, resultPath, "remove workload trigger", err)
	}
	if err := os.Remove(workPath); err != nil {
		p3ACCTestJobHelperFatal(t, resultPath, "remove workload file", err)
	}
	if err := p3ACCTestExerciseRealJobProcessListExpansion(beforeMembers, before); err != nil {
		p3ACCTestJobHelperFatal(t, resultPath, "exercise real greater-than-16 job list", err)
	}
	if err := os.WriteFile(resultPath, []byte("OK\n"), 0o600); err != nil {
		p3ACCTestJobHelperFatal(t, resultPath, "write success result", err)
	}
}

func TestP3ACCJobAccountingWorkloadProcess(t *testing.T) {
	if os.Getenv(p3ACCTestJobHelperModeEnvironment) != "workload" {
		t.Skip("nested workload helper only")
	}
	goPath := os.Getenv(p3ACCTestJobHelperGoEnvironment)
	workPath := os.Getenv(p3ACCTestJobHelperWorkEnvironment)
	readyPath := os.Getenv(p3ACCTestJobHelperReadyEnvironment)
	if !filepath.IsAbs(goPath) || !filepath.IsAbs(workPath) || !filepath.IsAbs(readyPath) {
		p3ACCTestJobHelperFatal(t, os.Getenv(p3ACCTestJobHelperResultEnvironment), "workload paths are not absolute", nil)
	}
	currentProcessID := uint32(os.Getpid())
	outerProcessID := os.Getenv(p3ACCTestJobHelperOuterEnvironment)
	var previousMembers []uint32
	stableDeadline := time.Now().Add(5 * time.Second)
	for {
		innerMembers, innerMembersOK := readP3ACCCurrentJobProcessIDs()
		innerAccounting, innerAccountingOK := readP3ACCCurrentJobAccounting()
		outerPresent := false
		for _, processID := range innerMembers {
			outerPresent = outerPresent || fmt.Sprintf("%d", processID) == outerProcessID
		}
		valid := innerMembersOK && innerAccountingOK &&
			p3ACCTestProcessIDsContain(innerMembers, currentProcessID) && !outerPresent &&
			int(innerAccounting.activeProcesses) == len(innerMembers)
		if valid && sameP3ACCJobProcessIDs(previousMembers, innerMembers) {
			break
		}
		if valid {
			previousMembers = append(previousMembers[:0], innerMembers...)
		} else {
			previousMembers = nil
		}
		if time.Now().After(stableDeadline) {
			p3ACCTestJobHelperFatal(
				t, os.Getenv(p3ACCTestJobHelperResultEnvironment), "hJob NULL did not resolve stable inner workload job", nil,
			)
		}
		time.Sleep(10 * time.Millisecond)
	}
	deadline := time.Now().Add(10 * time.Second)
	if err := os.WriteFile(readyPath, []byte("READY\n"), 0o600); err != nil {
		p3ACCTestJobHelperFatal(t, os.Getenv(p3ACCTestJobHelperResultEnvironment), "write workload ready marker", err)
	}
	for {
		if _, err := os.Stat(goPath); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			p3ACCTestJobHelperFatal(t, os.Getenv(p3ACCTestJobHelperResultEnvironment), "read workload trigger", err)
		}
		if time.Now().After(deadline) {
			p3ACCTestJobHelperFatal(t, os.Getenv(p3ACCTestJobHelperResultEnvironment), "workload trigger timed out", nil)
		}
		time.Sleep(10 * time.Millisecond)
	}
	buffer := make([]byte, 32<<20)
	for index := 0; index < len(buffer); index += 4_096 {
		buffer[index] = byte(index)
	}
	file, err := os.OpenFile(workPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		p3ACCTestJobHelperFatal(t, os.Getenv(p3ACCTestJobHelperResultEnvironment), "create workload file", err)
	}
	if _, err := file.Write(buffer); err != nil {
		_ = file.Close()
		p3ACCTestJobHelperFatal(t, os.Getenv(p3ACCTestJobHelperResultEnvironment), "write workload file", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		p3ACCTestJobHelperFatal(t, os.Getenv(p3ACCTestJobHelperResultEnvironment), "sync workload file", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		p3ACCTestJobHelperFatal(t, os.Getenv(p3ACCTestJobHelperResultEnvironment), "seek workload file", err)
	}
	if _, err := io.Copy(io.Discard, file); err != nil {
		_ = file.Close()
		p3ACCTestJobHelperFatal(t, os.Getenv(p3ACCTestJobHelperResultEnvironment), "read workload file", err)
	}
	if err := file.Close(); err != nil {
		p3ACCTestJobHelperFatal(t, os.Getenv(p3ACCTestJobHelperResultEnvironment), "close workload file", err)
	}
	var accumulator uint64
	var iterations uint64
	cpuDeadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(cpuDeadline) {
		for batch := 0; batch < 100_000; batch++ {
			iterations++
			accumulator = accumulator*33 + iterations
		}
	}
	if iterations == 0 {
		p3ACCTestJobHelperFatal(t, os.Getenv(p3ACCTestJobHelperResultEnvironment), "workload CPU loop did not run", nil)
	}
	p3ACCTestWorkloadSink = accumulator
}
