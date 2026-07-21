//go:build p3accacceptance && windows

package main

import (
	"errors"
	"math"
	"os"
	"sort"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	p3ACCKernel32                  = windows.NewLazySystemDLL("kernel32.dll")
	p3ACCGetProcessHandleCountProc = p3ACCKernel32.NewProc("GetProcessHandleCount")
	p3ACCPSAPI                     = windows.NewLazySystemDLL("psapi.dll")
	p3ACCGetProcessMemoryInfoProc  = p3ACCPSAPI.NewProc("GetProcessMemoryInfo")
	p3ACCGetSystemTimeAsFileTime   = windows.GetSystemTimeAsFileTime
	p3ACCCreateToolhelp32Snapshot  = windows.CreateToolhelp32Snapshot
	p3ACCProcess32First            = windows.Process32First
	p3ACCQueryJobInformation       = func(
		job windows.Handle,
		class int32,
		information unsafe.Pointer,
		informationLength uint32,
		returnedLength *uint32,
	) error {
		return windows.QueryInformationJobObject(
			job, class, uintptr(information), informationLength, returnedLength,
		)
	}
)

type p3ACCProcessMemoryCounters struct {
	Size                       uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
	PrivateUsage               uintptr
}

type p3ACCJobIOCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type p3ACCJobBasicAccountingInformation struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

type p3ACCJobBasicAndIOAccountingInformation struct {
	BasicInfo p3ACCJobBasicAccountingInformation
	IOInfo    p3ACCJobIOCounters
}

type p3ACCJobAccounting struct {
	cpu100NS                 int64
	pageFaults               int64
	readOperations           int64
	writeOperations          int64
	otherOperations          int64
	readBytes                int64
	writeBytes               int64
	otherBytes               int64
	totalProcesses           uint32
	activeProcesses          uint32
	totalTerminatedProcesses uint32
}

type p3ACCJobProcessIDListHeader struct {
	NumberOfAssignedProcesses uint32
	NumberOfProcessIDsInList  uint32
}

type p3ACCProcessIdentity struct {
	processID uint32
	createdAt uint64
}

type p3ACCProcessObservation struct {
	identity   p3ACCProcessIdentity
	threads    int64
	workingSet int64
	private    int64
	handles    int64
}

const (
	p3ACCJobProcessListInitialCapacity = 16
	p3ACCJobProcessListMaximum         = 256
)

var p3ACCProcessTreeTracker = struct {
	sync.Mutex
	initialized bool
	last        p3ACCJobAccounting
}{}

func resetP3ACCProcessTreeResourceTracker() {
	p3ACCProcessTreeTracker.Lock()
	p3ACCProcessTreeTracker.initialized = false
	p3ACCProcessTreeTracker.last = p3ACCJobAccounting{}
	p3ACCProcessTreeTracker.Unlock()
}

func readP3ACCProcessTreeResourceSample() p3ACCProcessTreeResourceSample {
	snapshotCutoff, cutoffOK := p3ACCProcessSnapshotCutoff()
	before, beforeOK := readP3ACCCurrentJobAccounting()
	firstMembers, firstMembersOK := readP3ACCCurrentJobProcessIDs()
	if !cutoffOK || !beforeOK || !firstMembersOK {
		return p3ACCProcessTreeResourceSample{}
	}
	observations, secondMembers, after, observationsOK := p3ACCObserveOwnedJobMembers(
		firstMembers, snapshotCutoff,
	)
	return p3ACCUpdateProcessTreeResourceTracker(
		observations, observationsOK, firstMembers, secondMembers, before, after,
	)
}

func p3ACCUpdateProcessTreeResourceTracker(
	observations []p3ACCProcessObservation,
	complete bool,
	firstMembers []uint32,
	secondMembers []uint32,
	before p3ACCJobAccounting,
	after p3ACCJobAccounting,
) p3ACCProcessTreeResourceSample {
	result := p3ACCProcessTreeResourceSample{Complete: complete}
	current := make(map[p3ACCProcessIdentity]struct{}, len(observations))
	for _, observation := range observations {
		if observation.identity.processID == 0 || observation.identity.createdAt == 0 ||
			observation.threads < 0 || observation.workingSet < 0 || observation.private < 0 ||
			observation.handles < 0 {
			result.Complete = false
			continue
		}
		if _, duplicate := current[observation.identity]; duplicate {
			result.Complete = false
			continue
		}
		current[observation.identity] = struct{}{}
		if !p3ACCCheckedAdd(&result.ProcessCount, 1) ||
			!p3ACCCheckedAdd(&result.WorkingSetBytes, observation.workingSet) ||
			!p3ACCCheckedAdd(&result.PrivateBytes, observation.private) ||
			!p3ACCCheckedAdd(&result.ThreadCount, observation.threads) ||
			!p3ACCCheckedAdd(&result.HandleCount, observation.handles) {
			result.Complete = false
		}
	}
	if !result.Complete || len(current) == 0 || len(current) != len(observations) ||
		!validP3ACCJobAccounting(before) || !validP3ACCJobAccounting(after) ||
		!sameP3ACCJobProcessIDs(firstMembers, secondMembers) ||
		len(firstMembers) != len(observations) ||
		before.totalProcesses != after.totalProcesses ||
		before.activeProcesses != after.activeProcesses ||
		before.totalTerminatedProcesses != after.totalTerminatedProcesses ||
		int(after.activeProcesses) != len(observations) ||
		!p3ACCJobAccountingAtLeast(after, before) {
		return p3ACCProcessTreeResourceSample{}
	}
	result.CPU100NS = after.cpu100NS
	result.ReadBytes = after.readBytes
	result.WriteBytes = after.writeBytes

	p3ACCProcessTreeTracker.Lock()
	defer p3ACCProcessTreeTracker.Unlock()
	if p3ACCProcessTreeTracker.initialized &&
		!p3ACCJobAccountingAtLeast(after, p3ACCProcessTreeTracker.last) {
		return p3ACCProcessTreeResourceSample{}
	}
	p3ACCProcessTreeTracker.initialized = true
	p3ACCProcessTreeTracker.last = after
	return result
}

func validP3ACCJobAccounting(value p3ACCJobAccounting) bool {
	return value.cpu100NS >= 0 && value.pageFaults >= 0 &&
		value.readOperations >= 0 && value.writeOperations >= 0 && value.otherOperations >= 0 &&
		value.readBytes >= 0 && value.writeBytes >= 0 && value.otherBytes >= 0 &&
		value.totalProcesses > 0 && value.activeProcesses > 0 &&
		value.activeProcesses <= value.totalProcesses &&
		value.totalTerminatedProcesses <= value.totalProcesses
}

func p3ACCJobAccountingAtLeast(value, baseline p3ACCJobAccounting) bool {
	return value.cpu100NS >= baseline.cpu100NS && value.pageFaults >= baseline.pageFaults &&
		value.readOperations >= baseline.readOperations &&
		value.writeOperations >= baseline.writeOperations &&
		value.otherOperations >= baseline.otherOperations &&
		value.readBytes >= baseline.readBytes && value.writeBytes >= baseline.writeBytes &&
		value.otherBytes >= baseline.otherBytes &&
		value.totalProcesses >= baseline.totalProcesses &&
		value.totalTerminatedProcesses >= baseline.totalTerminatedProcesses
}

// The tagged launcher assigns the app itself only to its protected outer Job.
// Therefore hJob == NULL resolves that immediate outer Job. Windows aggregates
// accounting and membership from the recorder's nested Job into this parent.
func readP3ACCCurrentJobAccounting() (p3ACCJobAccounting, bool) {
	information := p3ACCJobBasicAndIOAccountingInformation{}
	var returnedLength uint32
	expectedLength := uint32(unsafe.Sizeof(information))
	if expectedLength != 96 || unsafe.Offsetof(information.IOInfo) != 48 ||
		unsafe.Sizeof(p3ACCJobBasicAccountingInformation{}) != 48 ||
		unsafe.Sizeof(p3ACCJobIOCounters{}) != 48 || p3ACCQueryJobInformation(
		0,
		windows.JobObjectBasicAndIoAccountingInformation,
		unsafe.Pointer(&information),
		expectedLength,
		&returnedLength,
	) != nil || returnedLength != expectedLength {
		return p3ACCJobAccounting{}, false
	}
	cpu := int64(0)
	if information.BasicInfo.TotalUserTime < 0 || information.BasicInfo.TotalKernelTime < 0 ||
		information.BasicInfo.ThisPeriodTotalUserTime < 0 ||
		information.BasicInfo.ThisPeriodTotalKernelTime < 0 ||
		!p3ACCCheckedAdd(&cpu, information.BasicInfo.TotalUserTime) ||
		!p3ACCCheckedAdd(&cpu, information.BasicInfo.TotalKernelTime) {
		return p3ACCJobAccounting{}, false
	}
	pageFaults := int64(information.BasicInfo.TotalPageFaultCount)
	values := []uint64{
		information.IOInfo.ReadOperationCount,
		information.IOInfo.WriteOperationCount,
		information.IOInfo.OtherOperationCount,
		information.IOInfo.ReadTransferCount,
		information.IOInfo.WriteTransferCount,
		information.IOInfo.OtherTransferCount,
	}
	converted := make([]int64, len(values))
	for index, raw := range values {
		value, ok := p3ACCUnsignedToInt64(raw)
		if !ok {
			return p3ACCJobAccounting{}, false
		}
		converted[index] = value
	}
	result := p3ACCJobAccounting{
		cpu100NS: cpu, pageFaults: pageFaults,
		readOperations: converted[0], writeOperations: converted[1], otherOperations: converted[2],
		readBytes: converted[3], writeBytes: converted[4], otherBytes: converted[5],
		totalProcesses:           information.BasicInfo.TotalProcesses,
		activeProcesses:          information.BasicInfo.ActiveProcesses,
		totalTerminatedProcesses: information.BasicInfo.TotalTerminatedProcesses,
	}
	return result, validP3ACCJobAccounting(result)
}

func readP3ACCCurrentJobProcessIDs() ([]uint32, bool) {
	capacity := p3ACCJobProcessListInitialCapacity
	headerSize := int(unsafe.Sizeof(p3ACCJobProcessIDListHeader{}))
	pointerSize := int(unsafe.Sizeof(uintptr(0)))
	if headerSize != 8 || pointerSize != 8 {
		return nil, false
	}
	for capacity <= p3ACCJobProcessListMaximum {
		byteCount := headerSize + capacity*pointerSize
		wordCount := (byteCount + pointerSize - 1) / pointerSize
		buffer := make([]uintptr, wordCount)
		bufferBytes := uint32(wordCount * pointerSize)
		var returnedLength uint32
		err := p3ACCQueryJobInformation(
			0,
			windows.JobObjectBasicProcessIdList,
			unsafe.Pointer(&buffer[0]),
			bufferBytes,
			&returnedLength,
		)
		header := (*p3ACCJobProcessIDListHeader)(unsafe.Pointer(&buffer[0]))
		assigned := int(header.NumberOfAssignedProcesses)
		listed := int(header.NumberOfProcessIDsInList)
		if assigned < 0 || assigned > p3ACCJobProcessListMaximum || listed < 0 ||
			listed > assigned || listed > capacity ||
			(err != nil && !errors.Is(err, windows.ERROR_MORE_DATA)) {
			return nil, false
		}
		minimumReturnedLength := uint32(headerSize + listed*pointerSize)
		if err == nil && assigned == listed && listed > 0 {
			if returnedLength < minimumReturnedLength || returnedLength > bufferBytes {
				return nil, false
			}
			raw := unsafe.Slice(
				(*uintptr)(unsafe.Add(unsafe.Pointer(&buffer[0]), headerSize)), listed,
			)
			result := make([]uint32, 0, listed)
			seen := make(map[uint32]struct{}, listed)
			for _, processID := range raw {
				if processID == 0 || uint64(processID) > math.MaxUint32 {
					return nil, false
				}
				value := uint32(processID)
				if _, duplicate := seen[value]; duplicate {
					return nil, false
				}
				seen[value] = struct{}{}
				result = append(result, value)
			}
			sort.Slice(result, func(left, right int) bool { return result[left] < result[right] })
			return result, true
		}
		if assigned == listed {
			return nil, false
		}
		// A partial response is only an expansion fence: no PID is consumed yet.
		// Windows may report zero, a covered written length, or the exact required
		// length (which is larger than the supplied buffer on ERROR_MORE_DATA).
		requiredLengthValue := uint64(headerSize) + uint64(assigned)*uint64(pointerSize)
		if requiredLengthValue > math.MaxUint32 {
			return nil, false
		}
		requiredLength := uint32(requiredLengthValue)
		partialLengthValid := returnedLength == 0 || returnedLength == requiredLength ||
			(returnedLength >= minimumReturnedLength && returnedLength <= bufferBytes)
		if !partialLengthValid {
			return nil, false
		}
		needed := assigned
		if needed <= capacity {
			needed = capacity * 2
		}
		if needed > p3ACCJobProcessListMaximum {
			return nil, false
		}
		capacity = needed
	}
	return nil, false
}

func sameP3ACCJobProcessIDs(first, second []uint32) bool {
	if len(first) == 0 || len(first) != len(second) {
		return false
	}
	for index := range first {
		if first[index] == 0 || first[index] != second[index] ||
			(index > 0 && first[index-1] >= first[index]) {
			return false
		}
	}
	return true
}
func p3ACCObserveOwnedJobMembers(
	expected []uint32,
	snapshotCutoff uint64,
) ([]p3ACCProcessObservation, []uint32, p3ACCJobAccounting, bool) {
	entries, snapshotOK := p3ACCCaptureProcessEntries()
	if !snapshotOK || snapshotCutoff == 0 || len(expected) == 0 ||
		len(expected) > p3ACCJobProcessListMaximum {
		return nil, nil, p3ACCJobAccounting{}, false
	}
	root := uint32(os.Getpid())
	rootFound := false
	for _, processID := range expected {
		rootFound = rootFound || processID == root
	}
	if !rootFound {
		return nil, nil, p3ACCJobAccounting{}, false
	}
	type openedProcess struct {
		handle          windows.Handle
		identity        p3ACCProcessIdentity
		parentProcessID uint32
	}
	result := make([]p3ACCProcessObservation, 0, len(expected))
	opened := make([]openedProcess, 0, len(expected))
	defer func() {
		for _, process := range opened {
			_ = windows.CloseHandle(process.handle)
		}
	}()
	complete := true
	for _, processID := range expected {
		processEntry, exists := entries[processID]
		if !exists {
			complete = false
			continue
		}
		process, err := windows.OpenProcess(
			windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_VM_READ|windows.SYNCHRONIZE,
			false, processID,
		)
		if err != nil {
			complete = false
			continue
		}
		createdAt, creationOK := p3ACCProcessCreatedAt(process)
		workingSet, privateBytes, memoryOK := p3ACCProcessMemory(process)
		handles, handlesOK := p3ACCProcessHandles(process)
		if !creationOK || !memoryOK || !handlesOK || createdAt > snapshotCutoff {
			_ = windows.CloseHandle(process)
			complete = false
			continue
		}
		identity := p3ACCProcessIdentity{processID: processID, createdAt: createdAt}
		opened = append(opened, openedProcess{
			handle: process, identity: identity, parentProcessID: processEntry.ParentProcessID,
		})
		result = append(result, p3ACCProcessObservation{
			identity: identity, threads: int64(processEntry.Threads),
			workingSet: workingSet, private: privateBytes, handles: handles,
		})
	}
	secondMembers, secondMembersOK := readP3ACCCurrentJobProcessIDs()
	if !secondMembersOK || !sameP3ACCJobProcessIDs(expected, secondMembers) {
		complete = false
	}
	after, afterOK := readP3ACCCurrentJobAccounting()
	if !afterOK {
		complete = false
	}
	secondEntries, secondSnapshotOK := p3ACCCaptureProcessEntries()
	if secondSnapshotOK {
		for _, process := range opened {
			if !p3ACCVerifyProcessIdentity(
				process.handle, process.identity, process.parentProcessID,
				snapshotCutoff, secondEntries,
			) {
				complete = false
				break
			}
		}
	} else {
		complete = false
	}
	return result, secondMembers, after, complete && len(result) == len(expected)
}
func p3ACCProcessSnapshotCutoff() (uint64, bool) {
	var cutoff windows.Filetime
	p3ACCGetSystemTimeAsFileTime(&cutoff)
	value := uint64(cutoff.HighDateTime)<<32 | uint64(cutoff.LowDateTime)
	return value, value > 0
}

func p3ACCCaptureProcessSnapshot() (map[uint32]windows.ProcessEntry32, uint64, bool) {
	cutoff, cutoffOK := p3ACCProcessSnapshotCutoff()
	if !cutoffOK {
		return nil, 0, false
	}
	entries, snapshotOK := p3ACCCaptureProcessEntries()
	return entries, cutoff, snapshotOK
}

func p3ACCCaptureProcessEntries() (map[uint32]windows.ProcessEntry32, bool) {
	snapshot, err := p3ACCCreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, false
	}
	defer windows.CloseHandle(snapshot)
	entries := make(map[uint32]windows.ProcessEntry32)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	if err := p3ACCProcess32First(snapshot, &entry); err != nil {
		return nil, false
	}
	for {
		entries[entry.ProcessID] = entry
		err = windows.Process32Next(snapshot, &entry)
		if err != nil {
			break
		}
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, false
	}
	return entries, true
}
func p3ACCVerifyProcessIdentity(
	original windows.Handle,
	identity p3ACCProcessIdentity,
	parentProcessID uint32,
	snapshotCutoff uint64,
	secondEntries map[uint32]windows.ProcessEntry32,
) bool {
	entry, exists := secondEntries[identity.processID]
	if !exists || entry.ParentProcessID != parentProcessID ||
		identity.createdAt == 0 || identity.createdAt > snapshotCutoff {
		return false
	}
	originalProcessID, originalProcessIDErr := windows.GetProcessId(original)
	if originalProcessIDErr != nil || originalProcessID != identity.processID {
		return false
	}
	originalCreatedAt, originalOK := p3ACCProcessCreatedAt(original)
	if !originalOK || originalCreatedAt != identity.createdAt {
		return false
	}
	waitResult, waitErr := windows.WaitForSingleObject(original, 0)
	if waitErr != nil || waitResult != uint32(windows.WAIT_TIMEOUT) {
		return false
	}
	second, err := windows.OpenProcess(
		windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false,
		identity.processID,
	)
	if err != nil {
		return false
	}
	secondProcessID, secondProcessIDErr := windows.GetProcessId(second)
	secondCreatedAt, secondOK := p3ACCProcessCreatedAt(second)
	_ = windows.CloseHandle(second)
	return secondProcessIDErr == nil && secondProcessID == identity.processID && secondOK &&
		secondCreatedAt == identity.createdAt && secondCreatedAt <= snapshotCutoff
}

func p3ACCProcessCreatedAt(process windows.Handle) (uint64, bool) {
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(process, &creation, &exit, &kernel, &user); err != nil {
		return 0, false
	}
	createdAt := uint64(creation.HighDateTime)<<32 | uint64(creation.LowDateTime)
	return createdAt, createdAt > 0
}
func p3ACCProcessMemory(process windows.Handle) (int64, int64, bool) {
	counters := p3ACCProcessMemoryCounters{Size: uint32(unsafe.Sizeof(p3ACCProcessMemoryCounters{}))}
	result, _, _ := p3ACCGetProcessMemoryInfoProc.Call(
		uintptr(process), uintptr(unsafe.Pointer(&counters)), uintptr(counters.Size),
	)
	if result == 0 {
		return 0, 0, false
	}
	workingSet, workingSetOK := p3ACCUnsignedToInt64(uint64(counters.WorkingSetSize))
	privateBytes, privateOK := p3ACCUnsignedToInt64(uint64(counters.PrivateUsage))
	return workingSet, privateBytes, workingSetOK && privateOK
}

func p3ACCProcessHandles(process windows.Handle) (int64, bool) {
	var count uint32
	result, _, _ := p3ACCGetProcessHandleCountProc.Call(
		uintptr(process), uintptr(unsafe.Pointer(&count)),
	)
	if result == 0 {
		return 0, false
	}
	return int64(count), true
}

func p3ACCUnsignedToInt64(value uint64) (int64, bool) {
	if value > math.MaxInt64 {
		return 0, false
	}
	return int64(value), true
}

func p3ACCCheckedAdd(target *int64, value int64) bool {
	if target == nil || *target < 0 || value < 0 || value > math.MaxInt64-*target {
		return false
	}
	*target += value
	return true
}
