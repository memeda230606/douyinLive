Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$script:P3AccSentinelName = '.p3acc-controller-owned'
$script:P3AccSentinelText = "P3ACC-CONTROLLER/v1`n"
$script:P3AccSnapshotName = 'p3-acc.snapshot.json'
$script:P3AccSnapshotSchema = 'P3-ACC-001/v1'
$script:P3AccReportSchema = 'P3-ACC-CONTROLLER/v1'
$script:P3AccMaximumSnapshotBytes = 1MB
$script:P3AccProbeUri = 'https://github.com/robots.txt'

if ([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) {
    throw 'P3ACC_CONTROLLER_PLATFORM_INVALID'
}

if (-not ('P3Acc.NativePath' -as [type])) {
    Add-Type -TypeDefinition @'
using System;
using System.Collections.Generic;
using System.Drawing;
using System.Drawing.Imaging;
using System.IO;
using System.Runtime.InteropServices;
using System.Text;
using Microsoft.Win32.SafeHandles;

namespace P3Acc {
    public sealed class PathIdentity {
        public string FinalPath;
        public uint VolumeSerial;
        public uint FileIndexHigh;
        public uint FileIndexLow;
        public uint Attributes;
        public string Key { get { return VolumeSerial.ToString("X8") + ":" + FileIndexHigh.ToString("X8") + ":" + FileIndexLow.ToString("X8"); } }
    }

    public static class NativePath {
        private const uint FILE_READ_ATTRIBUTES = 0x80;
        private const uint FILE_SHARE_READ = 1;
        private const uint FILE_SHARE_WRITE = 2;
        private const uint FILE_SHARE_DELETE = 4;
        private const uint OPEN_EXISTING = 3;
        private const uint FILE_FLAG_BACKUP_SEMANTICS = 0x02000000;
        private const uint FILE_FLAG_OPEN_REPARSE_POINT = 0x00200000;
        private const uint FILE_ATTRIBUTE_DIRECTORY = 0x10;
        private const uint FILE_ATTRIBUTE_REPARSE_POINT = 0x400;

        [StructLayout(LayoutKind.Sequential)]
        private struct FILETIME { public uint Low; public uint High; }
        [StructLayout(LayoutKind.Sequential)]
        private struct BY_HANDLE_FILE_INFORMATION {
            public uint FileAttributes;
            public FILETIME CreationTime;
            public FILETIME LastAccessTime;
            public FILETIME LastWriteTime;
            public uint VolumeSerialNumber;
            public uint FileSizeHigh;
            public uint FileSizeLow;
            public uint NumberOfLinks;
            public uint FileIndexHigh;
            public uint FileIndexLow;
        }

        [DllImport("kernel32.dll", CharSet=CharSet.Unicode, SetLastError=true)]
        private static extern SafeFileHandle CreateFile(string name, uint access, uint share, IntPtr security, uint creation, uint flags, IntPtr template);
        [DllImport("kernel32.dll", SetLastError=true)]
        private static extern bool GetFileInformationByHandle(SafeFileHandle handle, out BY_HANDLE_FILE_INFORMATION info);
        [DllImport("kernel32.dll", CharSet=CharSet.Unicode, SetLastError=true)]
        private static extern uint GetFinalPathNameByHandle(SafeFileHandle handle, StringBuilder path, uint length, uint flags);

        public static PathIdentity Inspect(string path, bool directory) {
            uint flags = FILE_FLAG_OPEN_REPARSE_POINT | (directory ? FILE_FLAG_BACKUP_SEMANTICS : 0u);
            using (SafeFileHandle handle = CreateFile(path, FILE_READ_ATTRIBUTES, FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE, IntPtr.Zero, OPEN_EXISTING, flags, IntPtr.Zero)) {
                if (handle.IsInvalid) throw new IOException("P3ACC_CONTROLLER_PATH_INVALID");
                BY_HANDLE_FILE_INFORMATION info;
                if (!GetFileInformationByHandle(handle, out info)) throw new IOException("P3ACC_CONTROLLER_PATH_INVALID");
                if ((info.FileAttributes & FILE_ATTRIBUTE_REPARSE_POINT) != 0) throw new IOException("P3ACC_CONTROLLER_PATH_INVALID");
                bool isDirectory = (info.FileAttributes & FILE_ATTRIBUTE_DIRECTORY) != 0;
                if (isDirectory != directory) throw new IOException("P3ACC_CONTROLLER_PATH_INVALID");
                StringBuilder buffer = new StringBuilder(32768);
                uint count = GetFinalPathNameByHandle(handle, buffer, (uint)buffer.Capacity, 0);
                if (count == 0 || count >= buffer.Capacity) throw new IOException("P3ACC_CONTROLLER_PATH_INVALID");
                return new PathIdentity {
                    FinalPath = buffer.ToString(), VolumeSerial = info.VolumeSerialNumber,
                    FileIndexHigh = info.FileIndexHigh, FileIndexLow = info.FileIndexLow,
                    Attributes = info.FileAttributes
                };
            }
        }
        public static PathIdentity InspectHandle(SafeFileHandle handle, bool directory) {
            if (handle == null || handle.IsInvalid || handle.IsClosed) throw new IOException("P3ACC_CONTROLLER_PATH_INVALID");
            BY_HANDLE_FILE_INFORMATION info;
            if (!GetFileInformationByHandle(handle, out info)) throw new IOException("P3ACC_CONTROLLER_PATH_INVALID");
            if ((info.FileAttributes & FILE_ATTRIBUTE_REPARSE_POINT) != 0) throw new IOException("P3ACC_CONTROLLER_PATH_INVALID");
            bool isDirectory = (info.FileAttributes & FILE_ATTRIBUTE_DIRECTORY) != 0;
            if (isDirectory != directory) throw new IOException("P3ACC_CONTROLLER_PATH_INVALID");
            StringBuilder buffer = new StringBuilder(32768);
            uint count = GetFinalPathNameByHandle(handle, buffer, (uint)buffer.Capacity, 0);
            if (count == 0 || count >= buffer.Capacity) throw new IOException("P3ACC_CONTROLLER_PATH_INVALID");
            return new PathIdentity {
                FinalPath = buffer.ToString(), VolumeSerial = info.VolumeSerialNumber,
                FileIndexHigh = info.FileIndexHigh, FileIndexLow = info.FileIndexLow,
                Attributes = info.FileAttributes
            };
        }
    }

    public sealed class CaptureResult {
        public bool Captured;
        public bool NonUniform;
        public int Width;
        public int Height;
    }

    public static class NativeWindow {
        private delegate bool EnumWindowsProc(IntPtr hwnd, IntPtr parameter);
        [StructLayout(LayoutKind.Sequential)] private struct RECT { public int Left; public int Top; public int Right; public int Bottom; }
        [DllImport("user32.dll")] private static extern bool EnumWindows(EnumWindowsProc callback, IntPtr parameter);
        [DllImport("user32.dll")] private static extern bool IsWindowVisible(IntPtr hwnd);
        [DllImport("user32.dll")] private static extern uint GetWindowThreadProcessId(IntPtr hwnd, out uint processId);
        [DllImport("user32.dll")] private static extern bool GetWindowRect(IntPtr hwnd, out RECT rect);
        [DllImport("user32.dll")] private static extern bool PrintWindow(IntPtr hwnd, IntPtr hdc, uint flags);
        [DllImport("user32.dll")] private static extern bool PostMessage(IntPtr hwnd, uint message, IntPtr wparam, IntPtr lparam);
        private const uint PW_RENDERFULLCONTENT = 2;
        private const uint WM_CLOSE = 0x0010;

        private static List<IntPtr> Find(uint processId) {
            List<IntPtr> result = new List<IntPtr>();
            EnumWindows(delegate(IntPtr hwnd, IntPtr ignored) {
                uint owner;
                GetWindowThreadProcessId(hwnd, out owner);
                RECT rect;
                if (owner == processId && IsWindowVisible(hwnd) && GetWindowRect(hwnd, out rect) && rect.Right - rect.Left >= 640 && rect.Bottom - rect.Top >= 480) result.Add(hwnd);
                return true;
            }, IntPtr.Zero);
            return result;
        }

        public static CaptureResult CaptureSafeCrop(uint processId, string outputPath) {
            List<IntPtr> windows = Find(processId);
            if (windows.Count != 1) return new CaptureResult();
            RECT rect;
            if (!GetWindowRect(windows[0], out rect)) return new CaptureResult();
            int width = rect.Right - rect.Left;
            int height = rect.Bottom - rect.Top;
            using (Bitmap full = new Bitmap(width, height, PixelFormat.Format32bppArgb)) {
                using (Graphics graphics = Graphics.FromImage(full)) {
                    IntPtr hdc = graphics.GetHdc();
                    bool ok;
                    try { ok = PrintWindow(windows[0], hdc, PW_RENDERFULLCONTENT); }
                    finally { graphics.ReleaseHdc(hdc); }
                    if (!ok) return new CaptureResult();
                }
                int cropWidth = Math.Min(420, Math.Max(300, width * 30 / 100));
                int cropHeight = 180;
                int cropX = width - cropWidth;
                int cropY = Math.Min(100, height - cropHeight);
                Rectangle area = new Rectangle(cropX, cropY, cropWidth, cropHeight);
                using (Bitmap crop = full.Clone(area, PixelFormat.Format32bppArgb)) {
                    HashSet<int> colors = new HashSet<int>();
                    int xStep = Math.Max(1, crop.Width / 24);
                    int yStep = Math.Max(1, crop.Height / 12);
                    for (int y = 0; y < crop.Height; y += yStep) {
                        for (int x = 0; x < crop.Width; x += xStep) colors.Add(crop.GetPixel(x, y).ToArgb());
                    }
                    crop.Save(outputPath, ImageFormat.Png);
                    return new CaptureResult { Captured = true, NonUniform = colors.Count >= 3, Width = crop.Width, Height = crop.Height };
                }
            }
        }

        public static bool SendClose(uint processId) {
            List<IntPtr> windows = Find(processId);
            return windows.Count == 1 && PostMessage(windows[0], WM_CLOSE, IntPtr.Zero, IntPtr.Zero);
        }
    }

    public sealed class SafeJobHandle : SafeHandleZeroOrMinusOneIsInvalid {
        private SafeJobHandle() : base(true) { }
        protected override bool ReleaseHandle() { return NativeJob.CloseJobHandle(handle); }
    }

    public static class NativeJob {
        private const int ERROR_ALREADY_EXISTS = 183;
        private const uint DACL_SECURITY_INFORMATION = 0x00000004;
        private const uint LABEL_SECURITY_INFORMATION = 0x00000010;
        private const uint JOB_OBJECT_LIMIT_BREAKAWAY_OK = 0x00000800;
        private const uint JOB_OBJECT_LIMIT_SILENT_BREAKAWAY_OK = 0x00001000;
        private const uint JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE = 0x00002000;
        private const int JobObjectBasicAccountingInformation = 1;
        private const int JobObjectExtendedLimitInformation = 9;

        [StructLayout(LayoutKind.Sequential)]
        private struct SECURITY_ATTRIBUTES {
            public int nLength;
            public IntPtr lpSecurityDescriptor;
            public int bInheritHandle;
        }

        [StructLayout(LayoutKind.Sequential)]
        private struct IO_COUNTERS {
            public ulong ReadOperationCount;
            public ulong WriteOperationCount;
            public ulong OtherOperationCount;
            public ulong ReadTransferCount;
            public ulong WriteTransferCount;
            public ulong OtherTransferCount;
        }

        [StructLayout(LayoutKind.Sequential)]
        private struct JOBOBJECT_BASIC_LIMIT_INFORMATION {
            public long PerProcessUserTimeLimit;
            public long PerJobUserTimeLimit;
            public uint LimitFlags;
            public UIntPtr MinimumWorkingSetSize;
            public UIntPtr MaximumWorkingSetSize;
            public uint ActiveProcessLimit;
            public UIntPtr Affinity;
            public uint PriorityClass;
            public uint SchedulingClass;
        }

        [StructLayout(LayoutKind.Sequential)]
        private struct JOBOBJECT_EXTENDED_LIMIT_INFORMATION {
            public JOBOBJECT_BASIC_LIMIT_INFORMATION BasicLimitInformation;
            public IO_COUNTERS IoInfo;
            public UIntPtr ProcessMemoryLimit;
            public UIntPtr JobMemoryLimit;
            public UIntPtr PeakProcessMemoryUsed;
            public UIntPtr PeakJobMemoryUsed;
        }

        [StructLayout(LayoutKind.Sequential)]
        private struct JOBOBJECT_BASIC_ACCOUNTING_INFORMATION {
            public long TotalUserTime;
            public long TotalKernelTime;
            public long ThisPeriodTotalUserTime;
            public long ThisPeriodTotalKernelTime;
            public uint TotalPageFaultCount;
            public uint TotalProcesses;
            public uint ActiveProcesses;
            public uint TotalTerminatedProcesses;
        }

        [DllImport("kernel32.dll", CharSet=CharSet.Unicode, SetLastError=true)]
        private static extern SafeJobHandle CreateJobObject(ref SECURITY_ATTRIBUTES attributes, string name);
        [DllImport("kernel32.dll", SetLastError=true)]
        private static extern bool SetInformationJobObject(SafeJobHandle job, int infoClass, IntPtr info, uint length);
        [DllImport("kernel32.dll", SetLastError=true)]
        private static extern bool QueryInformationJobObject(SafeJobHandle job, int infoClass, IntPtr info, uint length, IntPtr returnLength);
        [DllImport("kernel32.dll", SetLastError=true)]
        private static extern bool IsProcessInJob(IntPtr process, SafeJobHandle job, out bool result);
        [DllImport("kernel32.dll", SetLastError=true)]
        private static extern bool AssignProcessToJobObject(SafeJobHandle job, IntPtr process);
        [DllImport("kernel32.dll", SetLastError=true)]
        private static extern bool TerminateJobObject(SafeJobHandle job, uint exitCode);
        [DllImport("advapi32.dll", SetLastError=true)]
        private static extern bool GetKernelObjectSecurity(SafeJobHandle handle, uint securityInformation, byte[] descriptor, uint length, out uint needed);
        [DllImport("kernel32.dll", SetLastError=true)]
        internal static extern bool CloseHandle(IntPtr handle);

        internal static bool CloseJobHandle(IntPtr handle) { return CloseHandle(handle); }

        private static void ThrowWin32(string code) {
            throw new System.ComponentModel.Win32Exception(Marshal.GetLastWin32Error(), code);
        }

        public static SafeJobHandle CreateOwned(string name, string currentUserSid) {
            if (String.IsNullOrWhiteSpace(name) || String.IsNullOrWhiteSpace(currentUserSid)) throw new ArgumentException("P3ACC_JOB_INVALID");
            System.Security.Principal.SecurityIdentifier current = new System.Security.Principal.SecurityIdentifier(currentUserSid);
            string sddl = "O:" + current.Value + "G:" + current.Value + "D:P(A;;GA;;;SY)(A;;GA;;;" + current.Value + ")S:P(ML;;NW;;;ME)";
            System.Security.AccessControl.RawSecurityDescriptor raw = new System.Security.AccessControl.RawSecurityDescriptor(sddl);
            byte[] descriptor = new byte[raw.BinaryLength];
            raw.GetBinaryForm(descriptor, 0);
            GCHandle pin = GCHandle.Alloc(descriptor, GCHandleType.Pinned);
            SafeJobHandle job = null;
            try {
                SECURITY_ATTRIBUTES attributes = new SECURITY_ATTRIBUTES();
                attributes.nLength = Marshal.SizeOf(typeof(SECURITY_ATTRIBUTES));
                attributes.lpSecurityDescriptor = pin.AddrOfPinnedObject();
                attributes.bInheritHandle = 0;
                job = CreateJobObject(ref attributes, name);
                int createError = Marshal.GetLastWin32Error();
                if (job == null || job.IsInvalid) ThrowWin32("P3ACC_JOB_CREATE_FAILED");
                if (createError == ERROR_ALREADY_EXISTS) {
                    job.Dispose();
                    throw new InvalidOperationException("P3ACC_JOB_NAME_COLLISION");
                }
                JOBOBJECT_EXTENDED_LIMIT_INFORMATION limits = new JOBOBJECT_EXTENDED_LIMIT_INFORMATION();
                limits.BasicLimitInformation.LimitFlags = JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE;
                int size = Marshal.SizeOf(typeof(JOBOBJECT_EXTENDED_LIMIT_INFORMATION));
                IntPtr buffer = Marshal.AllocHGlobal(size);
                try {
                    Marshal.StructureToPtr(limits, buffer, false);
                    if (!SetInformationJobObject(job, JobObjectExtendedLimitInformation, buffer, (uint)size)) ThrowWin32("P3ACC_JOB_LIMIT_FAILED");
                } finally { Marshal.FreeHGlobal(buffer); }
                uint actual = GetLimitFlags(job);
                if (actual != JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE ||
                    (actual & (JOB_OBJECT_LIMIT_BREAKAWAY_OK | JOB_OBJECT_LIMIT_SILENT_BREAKAWAY_OK)) != 0) {
                    throw new InvalidOperationException("P3ACC_JOB_LIMIT_FAILED");
                }
                if (!HasExactProtectedDacl(job, current.Value)) throw new InvalidOperationException("P3ACC_JOB_DACL_FAILED");
                if (!HasExactMediumMandatoryLabel(job)) throw new InvalidOperationException("P3ACC_JOB_LABEL_FAILED");
                return job;
            } catch {
                if (job != null) job.Dispose();
                throw;
            } finally { pin.Free(); }
        }

        public static uint GetLimitFlags(SafeJobHandle job) {
            int size = Marshal.SizeOf(typeof(JOBOBJECT_EXTENDED_LIMIT_INFORMATION));
            IntPtr buffer = Marshal.AllocHGlobal(size);
            try {
                if (!QueryInformationJobObject(job, JobObjectExtendedLimitInformation, buffer, (uint)size, IntPtr.Zero)) ThrowWin32("P3ACC_JOB_QUERY_FAILED");
                return ((JOBOBJECT_EXTENDED_LIMIT_INFORMATION)Marshal.PtrToStructure(buffer, typeof(JOBOBJECT_EXTENDED_LIMIT_INFORMATION))).BasicLimitInformation.LimitFlags;
            } finally { Marshal.FreeHGlobal(buffer); }
        }

        public static uint GetActiveProcesses(SafeJobHandle job) {
            int size = Marshal.SizeOf(typeof(JOBOBJECT_BASIC_ACCOUNTING_INFORMATION));
            IntPtr buffer = Marshal.AllocHGlobal(size);
            try {
                if (!QueryInformationJobObject(job, JobObjectBasicAccountingInformation, buffer, (uint)size, IntPtr.Zero)) ThrowWin32("P3ACC_JOB_QUERY_FAILED");
                return ((JOBOBJECT_BASIC_ACCOUNTING_INFORMATION)Marshal.PtrToStructure(buffer, typeof(JOBOBJECT_BASIC_ACCOUNTING_INFORMATION))).ActiveProcesses;
            } finally { Marshal.FreeHGlobal(buffer); }
        }

        public static bool ContainsProcess(SafeJobHandle job, IntPtr processHandle) {
            bool result;
            if (!IsProcessInJob(processHandle, job, out result)) ThrowWin32("P3ACC_JOB_QUERY_FAILED");
            return result;
        }

        public static void AssignProcess(SafeJobHandle job, IntPtr processHandle) {
            if (!AssignProcessToJobObject(job, processHandle)) ThrowWin32("P3ACC_JOB_ASSIGN_FAILED");
        }

        public static void Terminate(SafeJobHandle job, uint exitCode) {
            if (!TerminateJobObject(job, exitCode)) ThrowWin32("P3ACC_JOB_TERMINATE_FAILED");
        }

        public static bool HasExactProtectedDacl(SafeJobHandle job, string currentUserSid) {
            uint needed;
            GetKernelObjectSecurity(job, DACL_SECURITY_INFORMATION, null, 0, out needed);
            if (needed == 0) ThrowWin32("P3ACC_JOB_DACL_FAILED");
            byte[] descriptor = new byte[needed];
            if (!GetKernelObjectSecurity(job, DACL_SECURITY_INFORMATION, descriptor, needed, out needed)) ThrowWin32("P3ACC_JOB_DACL_FAILED");
            System.Security.AccessControl.RawSecurityDescriptor raw = new System.Security.AccessControl.RawSecurityDescriptor(descriptor, 0);
            if ((raw.ControlFlags & System.Security.AccessControl.ControlFlags.DiscretionaryAclProtected) == 0 || raw.DiscretionaryAcl == null || raw.DiscretionaryAcl.Count != 2) return false;
            HashSet<string> expected = new HashSet<string>(StringComparer.OrdinalIgnoreCase);
            expected.Add(currentUserSid);
            expected.Add(new System.Security.Principal.SecurityIdentifier(System.Security.Principal.WellKnownSidType.LocalSystemSid, null).Value);
            foreach (System.Security.AccessControl.GenericAce genericAce in raw.DiscretionaryAcl) {
                System.Security.AccessControl.CommonAce ace = genericAce as System.Security.AccessControl.CommonAce;
                if (ace == null || ace.AceQualifier != System.Security.AccessControl.AceQualifier.AccessAllowed || ace.IsInherited || !expected.Remove(ace.SecurityIdentifier.Value)) return false;
            }
            return expected.Count == 0;
        }

        public static bool HasExactMediumMandatoryLabel(SafeJobHandle job) {
            uint needed;
            GetKernelObjectSecurity(job, LABEL_SECURITY_INFORMATION, null, 0, out needed);
            if (needed == 0) ThrowWin32("P3ACC_JOB_LABEL_FAILED");
            byte[] descriptor = new byte[needed];
            if (!GetKernelObjectSecurity(job, LABEL_SECURITY_INFORMATION, descriptor, needed, out needed)) ThrowWin32("P3ACC_JOB_LABEL_FAILED");
            System.Security.AccessControl.RawSecurityDescriptor raw = new System.Security.AccessControl.RawSecurityDescriptor(descriptor, 0);
            if ((raw.ControlFlags & System.Security.AccessControl.ControlFlags.SystemAclPresent) == 0 ||
                (raw.ControlFlags & System.Security.AccessControl.ControlFlags.SystemAclProtected) == 0 ||
                raw.SystemAcl == null || raw.SystemAcl.Count != 1) return false;
            System.Security.AccessControl.CustomAce ace = raw.SystemAcl[0] as System.Security.AccessControl.CustomAce;
            if (ace == null || (byte)ace.AceType != 0x11 || ace.AceFlags != System.Security.AccessControl.AceFlags.None) return false;
            byte[] opaque = ace.GetOpaque();
            if (opaque == null || opaque.Length != 16 || BitConverter.ToUInt32(opaque, 0) != 1) return false;
            try {
                System.Security.Principal.SecurityIdentifier label = new System.Security.Principal.SecurityIdentifier(opaque, 4);
                return label.BinaryLength == opaque.Length - 4 && label.Value == "S-1-16-8192";
            } catch { return false; }
        }
    }
}
'@ -ReferencedAssemblies 'System.Drawing'
}

function Throw-P3AccFailure {
    param([Parameter(Mandatory)][string]$Code)
    throw $Code
}

function Get-P3AccFailureCode {
    param([Parameter(Mandatory)]$ErrorRecord)
    $allowed = @(
        'P3ACC_CONTROLLER_PLATFORM_INVALID', 'P3ACC_CONTROLLER_CONFIG_INVALID',
        'P3ACC_CONTROLLER_ROOT_INVALID', 'P3ACC_CONTROLLER_SECRET_INVALID',
        'P3ACC_CONTROLLER_RELAY_FAILED', 'P3ACC_CONTROLLER_APP_LAUNCH_FAILED',
        'P3ACC_CONTROLLER_SNAPSHOT_INVALID', 'P3ACC_CONTROLLER_TIMEOUT',
        'P3ACC_CONTROLLER_TOPOLOGY_INVALID', 'P3ACC_CONTROLLER_PROBE_FAILED',
        'P3ACC_CONTROLLER_FINAL_INVALID', 'P3ACC_CONTROLLER_VISUAL_FAILED',
        'P3ACC_CONTROLLER_CLEANUP_FAILED'
    )
    $message = [string]$ErrorRecord.Exception.Message
    if ($allowed -contains $message) { return $message }
    return 'P3ACC_CONTROLLER_INTERNAL_ERROR'
}

function Convert-P3AccFinalPath {
    param([Parameter(Mandatory)][string]$Path)
    if ($Path.StartsWith('\\?\UNC\', [StringComparison]::OrdinalIgnoreCase)) {
        return '\\' + $Path.Substring(8)
    }
    if ($Path.StartsWith('\\?\', [StringComparison]::OrdinalIgnoreCase)) {
        return $Path.Substring(4)
    }
    return $Path
}

function Get-P3AccFullPath {
    param([Parameter(Mandatory)][string]$Path)
    if ([string]::IsNullOrWhiteSpace($Path) -or $Path.IndexOf([char]0) -ge 0 -or $Path.Contains('%')) {
        Throw-P3AccFailure 'P3ACC_CONTROLLER_CONFIG_INVALID'
    }
    try { $full = [IO.Path]::GetFullPath($Path) }
    catch { Throw-P3AccFailure 'P3ACC_CONTROLLER_CONFIG_INVALID' }
    if (-not [IO.Path]::IsPathRooted($full) -or $full.StartsWith('\\')) {
        Throw-P3AccFailure 'P3ACC_CONTROLLER_CONFIG_INVALID'
    }
    return $full.TrimEnd('\')
}

function Assert-P3AccNoReparsePath {
    param(
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][bool]$Directory
    )
    $full = Get-P3AccFullPath $Path
    $volumeRoot = [IO.Path]::GetPathRoot($full)
    if ([string]::IsNullOrWhiteSpace($volumeRoot)) { Throw-P3AccFailure 'P3ACC_CONTROLLER_CONFIG_INVALID' }
    $relative = $full.Substring($volumeRoot.Length).Trim('\')
    $current = $volumeRoot.TrimEnd('\') + '\'
    [void][P3Acc.NativePath]::Inspect($current, $true)
    if ($relative.Length -gt 0) {
        $parts = $relative.Split('\')
        for ($index = 0; $index -lt $parts.Count; $index++) {
            if ([string]::IsNullOrWhiteSpace($parts[$index]) -or $parts[$index] -eq '.' -or $parts[$index] -eq '..') {
                Throw-P3AccFailure 'P3ACC_CONTROLLER_CONFIG_INVALID'
            }
            $current = [IO.Path]::Combine($current, $parts[$index])
            $isDirectory = $Directory -or $index -lt $parts.Count - 1
            try { [void][P3Acc.NativePath]::Inspect($current, $isDirectory) }
            catch { Throw-P3AccFailure 'P3ACC_CONTROLLER_CONFIG_INVALID' }
        }
    }
    try { $identity = [P3Acc.NativePath]::Inspect($full, $Directory) }
    catch { Throw-P3AccFailure 'P3ACC_CONTROLLER_CONFIG_INVALID' }
    $canonical = Convert-P3AccFinalPath $identity.FinalPath
    if (-not [string]::Equals((Get-P3AccFullPath $canonical), $full, [StringComparison]::OrdinalIgnoreCase)) {
        Throw-P3AccFailure 'P3ACC_CONTROLLER_CONFIG_INVALID'
    }
    return [pscustomobject]@{ Path = $full; Canonical = $canonical; Identity = $identity.Key }
}

function Test-P3AccBytesEqual {
    param([byte[]]$Left, [byte[]]$Right)
    if ($null -eq $Left -or $null -eq $Right -or $Left.Length -ne $Right.Length) { return $false }
    for ($index = 0; $index -lt $Left.Length; $index++) {
        if ($Left[$index] -ne $Right[$index]) { return $false }
    }
    return $true
}

function Assert-P3AccSentinel {
    param([Parameter(Mandatory)][string]$Root)
    $sentinel = [IO.Path]::Combine($Root, $script:P3AccSentinelName)
    $sentinelInfo = Assert-P3AccNoReparsePath -Path $sentinel -Directory $false
    $expected = [Text.UTF8Encoding]::new($false).GetBytes($script:P3AccSentinelText)
    try { $actual = [IO.File]::ReadAllBytes($sentinelInfo.Path) }
    catch { Throw-P3AccFailure 'P3ACC_CONTROLLER_ROOT_INVALID' }
    if (-not (Test-P3AccBytesEqual $actual $expected)) { Throw-P3AccFailure 'P3ACC_CONTROLLER_ROOT_INVALID' }
}

function Assert-P3AccRunRoot {
    param([Parameter(Mandatory)][string]$Root)
    try { $rootInfo = Assert-P3AccNoReparsePath -Path $Root -Directory $true }
    catch { Throw-P3AccFailure 'P3ACC_CONTROLLER_ROOT_INVALID' }
    $volumeRoot = [IO.Path]::GetPathRoot($rootInfo.Path)
    $relative = $rootInfo.Path.Substring($volumeRoot.Length).Trim('\')
    $parts = @($relative.Split('\') | Where-Object { $_ -ne '' })
    if ($parts.Count -lt 2 -or [string]::Equals($rootInfo.Path.TrimEnd('\'), $volumeRoot.TrimEnd('\'), [StringComparison]::OrdinalIgnoreCase)) {
        Throw-P3AccFailure 'P3ACC_CONTROLLER_ROOT_INVALID'
    }
    $entries = @(Get-ChildItem -LiteralPath $rootInfo.Path -Force -ErrorAction Stop)
    if ($entries.Count -ne 1 -or $entries[0].Name -cne $script:P3AccSentinelName -or $entries[0].PSIsContainer) {
        Throw-P3AccFailure 'P3ACC_CONTROLLER_ROOT_INVALID'
    }
    Assert-P3AccSentinel $rootInfo.Path
    return $rootInfo
}

function Assert-P3AccRootStillOwned {
    param([Parameter(Mandatory)]$Configuration)
    try { $now = Assert-P3AccNoReparsePath -Path $Configuration.Root -Directory $true }
    catch { Throw-P3AccFailure 'P3ACC_CONTROLLER_ROOT_INVALID' }
    if ($now.Identity -cne $Configuration.RootIdentity -or
        -not [string]::Equals($now.Canonical, $Configuration.RootCanonical, [StringComparison]::OrdinalIgnoreCase)) {
        Throw-P3AccFailure 'P3ACC_CONTROLLER_ROOT_INVALID'
    }
    Assert-P3AccSentinel $Configuration.Root
}

function Assert-P3AccControlRootStillOwned {
    param([Parameter(Mandatory)]$Configuration)
    try { $now = Assert-P3AccNoReparsePath -Path $Configuration.ControlRoot -Directory $true }
    catch { Throw-P3AccFailure 'P3ACC_CONTROLLER_ROOT_INVALID' }
    if ($now.Identity -cne $Configuration.ControlRootIdentity -or
        -not [string]::Equals($now.Canonical, $Configuration.ControlRootCanonical, [StringComparison]::OrdinalIgnoreCase)) {
        Throw-P3AccFailure 'P3ACC_CONTROLLER_ROOT_INVALID'
    }
    return $now
}

function Assert-P3AccExecutable {
    param([Parameter(Mandatory)][string]$Path)
    try { $info = Assert-P3AccNoReparsePath -Path $Path -Directory $false }
    catch { Throw-P3AccFailure 'P3ACC_CONTROLLER_CONFIG_INVALID' }
    if ([IO.Path]::GetExtension($info.Path) -ine '.exe' -or (Get-Item -LiteralPath $info.Path -Force).Length -le 0) {
        Throw-P3AccFailure 'P3ACC_CONTROLLER_CONFIG_INVALID'
    }
    return $info.Path
}

function Test-P3AccPathWithin {
    param([string]$Root, [string]$Candidate)
    $prefix = $Root.TrimEnd('\') + '\'
    return $Candidate.StartsWith($prefix, [StringComparison]::OrdinalIgnoreCase)
}

function Assert-P3AccSecretFile {
    param([Parameter(Mandatory)][string]$Path, [Parameter(Mandatory)][string]$Root)
    try { $info = Assert-P3AccNoReparsePath -Path $Path -Directory $false }
    catch { Throw-P3AccFailure 'P3ACC_CONTROLLER_SECRET_INVALID' }
    if (Test-P3AccPathWithin -Root $Root -Candidate $info.Path) { Throw-P3AccFailure 'P3ACC_CONTROLLER_SECRET_INVALID' }
    $length = (Get-Item -LiteralPath $info.Path -Force -ErrorAction Stop).Length
    if ($length -lt 1 -or $length -gt 8192) { Throw-P3AccFailure 'P3ACC_CONTROLLER_SECRET_INVALID' }
    try { $acl = Get-Acl -LiteralPath $info.Path -ErrorAction Stop }
    catch { Throw-P3AccFailure 'P3ACC_CONTROLLER_SECRET_INVALID' }
    if (-not $acl.AreAccessRulesProtected) { Throw-P3AccFailure 'P3ACC_CONTROLLER_SECRET_INVALID' }
    $currentSid = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
    $systemSid = 'S-1-5-18'
    try { $ownerSid = ([Security.Principal.NTAccount]$acl.Owner).Translate([Security.Principal.SecurityIdentifier]).Value }
    catch { Throw-P3AccFailure 'P3ACC_CONTROLLER_SECRET_INVALID' }
    if ($ownerSid -cne $currentSid) { Throw-P3AccFailure 'P3ACC_CONTROLLER_SECRET_INVALID' }
    $currentCanRead = $false
    foreach ($rule in $acl.GetAccessRules($true, $true, [Security.Principal.SecurityIdentifier])) {
        $sid = $rule.IdentityReference.Value
        if ($rule.AccessControlType -eq [Security.AccessControl.AccessControlType]::Allow) {
            if ($sid -cne $currentSid -and $sid -cne $systemSid) { Throw-P3AccFailure 'P3ACC_CONTROLLER_SECRET_INVALID' }
            if ($sid -ceq $currentSid -and (($rule.FileSystemRights -band [Security.AccessControl.FileSystemRights]::ReadData) -ne 0)) {
                $currentCanRead = $true
            }
        }
    }
    if (-not $currentCanRead) { Throw-P3AccFailure 'P3ACC_CONTROLLER_SECRET_INVALID' }
    return [pscustomobject]@{ Path = $info.Path; Identity = $info.Identity }
}

function New-P3AccPrivateControlRoot {
    param([Parameter(Mandatory)][string]$AcceptanceRoot)
    $parent = [IO.Path]::GetDirectoryName($AcceptanceRoot)
    try { [void](Assert-P3AccNoReparsePath -Path $parent -Directory $true) }
    catch { Throw-P3AccFailure 'P3ACC_CONTROLLER_ROOT_INVALID' }
    $controlRoot = [IO.Path]::Combine($parent, '.p3acc-control-' + [Guid]::NewGuid().ToString('N'))
    if (Test-Path -LiteralPath $controlRoot) { Throw-P3AccFailure 'P3ACC_CONTROLLER_ROOT_INVALID' }
    try {
        [void][IO.Directory]::CreateDirectory($controlRoot)
        $currentSid = [Security.Principal.WindowsIdentity]::GetCurrent().User
        $systemSid = [Security.Principal.SecurityIdentifier]::new('S-1-5-18')
        $security = [Security.AccessControl.DirectorySecurity]::new()
        $security.SetOwner($currentSid)
        $security.SetAccessRuleProtection($true, $false)
        $inheritance = [Security.AccessControl.InheritanceFlags]::ContainerInherit -bor [Security.AccessControl.InheritanceFlags]::ObjectInherit
        $propagation = [Security.AccessControl.PropagationFlags]::None
        $allow = [Security.AccessControl.AccessControlType]::Allow
        $currentRule = [Security.AccessControl.FileSystemAccessRule]::new($currentSid, [Security.AccessControl.FileSystemRights]::FullControl, $inheritance, $propagation, $allow)
        $systemRule = [Security.AccessControl.FileSystemAccessRule]::new($systemSid, [Security.AccessControl.FileSystemRights]::FullControl, $inheritance, $propagation, $allow)
        [void]$security.AddAccessRule($currentRule)
        [void]$security.AddAccessRule($systemRule)
        [IO.Directory]::SetAccessControl($controlRoot, $security)
        $info = Assert-P3AccNoReparsePath -Path $controlRoot -Directory $true
        $entries = @(Get-ChildItem -LiteralPath $controlRoot -Force -ErrorAction Stop)
        if ($entries.Count -ne 0) { Throw-P3AccFailure 'P3ACC_CONTROLLER_ROOT_INVALID' }
        return $info
    } catch {
        if (Test-Path -LiteralPath $controlRoot -PathType Container) {
            Remove-Item -LiteralPath $controlRoot -Recurse -Force -ErrorAction SilentlyContinue
        }
        Throw-P3AccFailure 'P3ACC_CONTROLLER_ROOT_INVALID'
    }
}

function ConvertFrom-P3AccLoopbackEndpoint {
    param([Parameter(Mandatory)][string]$Value)
    $hostText = $null
    $portText = $null
    if ($Value -match '^(127(?:\.[0-9]{1,3}){3}):([0-9]{1,5})$') {
        $hostText, $portText = $Matches[1], $Matches[2]
    } elseif ($Value -match '^\[(::1)\]:([0-9]{1,5})$') {
        $hostText, $portText = $Matches[1], $Matches[2]
    } else {
        Throw-P3AccFailure 'P3ACC_CONTROLLER_CONFIG_INVALID'
    }
    try { $address = [Net.IPAddress]::Parse($hostText) }
    catch { Throw-P3AccFailure 'P3ACC_CONTROLLER_CONFIG_INVALID' }
    $port = [int]$portText
    if (-not [Net.IPAddress]::IsLoopback($address) -or $port -lt 1 -or $port -gt 65535) {
        Throw-P3AccFailure 'P3ACC_CONTROLLER_CONFIG_INVALID'
    }
    return [pscustomobject]@{ Host = $address.ToString(); Port = $port; Canonical = $Value }
}

function Assert-P3AccTimeout {
    param([int]$Value, [int]$Minimum, [int]$Maximum)
    if ($Value -lt $Minimum -or $Value -gt $Maximum) { Throw-P3AccFailure 'P3ACC_CONTROLLER_CONFIG_INVALID' }
}

function New-P3AccControllerConfiguration {
    param(
        [string]$AppExecutable, [string]$LauncherExecutable, [string]$RelayExecutable, [string]$Root,
        [string]$LiveUrlFile, [string]$ClashUpstream,
        [int]$StartupTimeoutSeconds, [int]$PreFaultTimeoutSeconds,
        [int]$FaultDetectionTimeoutSeconds, [int]$RecoveryTimeoutSeconds,
        [int]$FinalizationTimeoutSeconds, [int]$CloseTimeoutSeconds,
        [int]$ProbeTimeoutSeconds, [int]$PollIntervalMilliseconds
    )
    Assert-P3AccTimeout $StartupTimeoutSeconds 30 900
    Assert-P3AccTimeout $PreFaultTimeoutSeconds 660 7200
    Assert-P3AccTimeout $FaultDetectionTimeoutSeconds 30 900
    Assert-P3AccTimeout $RecoveryTimeoutSeconds 30 1800
    Assert-P3AccTimeout $FinalizationTimeoutSeconds 60 43200
    Assert-P3AccTimeout $CloseTimeoutSeconds 5 120
    Assert-P3AccTimeout $ProbeTimeoutSeconds 2 30
    if ($PollIntervalMilliseconds -lt 250 -or $PollIntervalMilliseconds -gt 5000) { Throw-P3AccFailure 'P3ACC_CONTROLLER_CONFIG_INVALID' }
    $rootInfo = Assert-P3AccRunRoot $Root
    $app = Assert-P3AccExecutable $AppExecutable
    $launcher = Assert-P3AccExecutable $LauncherExecutable
    $relay = Assert-P3AccExecutable $RelayExecutable
    if ([string]::Equals($app, $launcher, [StringComparison]::OrdinalIgnoreCase) -or
        [string]::Equals($app, $relay, [StringComparison]::OrdinalIgnoreCase) -or
        [string]::Equals($launcher, $relay, [StringComparison]::OrdinalIgnoreCase)) { Throw-P3AccFailure 'P3ACC_CONTROLLER_CONFIG_INVALID' }
    $secret = Assert-P3AccSecretFile -Path $LiveUrlFile -Root $rootInfo.Path
    $upstream = ConvertFrom-P3AccLoopbackEndpoint $ClashUpstream
    $dataPath = [IO.Path]::Combine($rootInfo.Path, 'data')
    $resultDirectory = [IO.Path]::Combine($rootInfo.Path, 'result')
    $resultPath = [IO.Path]::Combine($resultDirectory, $script:P3AccSnapshotName)
    if ((Test-Path -LiteralPath $dataPath) -or (Test-Path -LiteralPath $resultDirectory) -or (Test-Path -LiteralPath $resultPath)) { Throw-P3AccFailure 'P3ACC_CONTROLLER_ROOT_INVALID' }
    $sqliteCommand = Get-Command sqlite3.exe -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($null -eq $sqliteCommand) { Throw-P3AccFailure 'P3ACC_CONTROLLER_CONFIG_INVALID' }
    $sqlite = Assert-P3AccExecutable $sqliteCommand.Source
    $controlRoot = New-P3AccPrivateControlRoot $rootInfo.Path
    return [pscustomobject]@{
        AppExecutable = $app; LauncherExecutable = $launcher; RelayExecutable = $relay; Root = $rootInfo.Path
        RootCanonical = $rootInfo.Canonical; RootIdentity = $rootInfo.Identity
        ControlRoot = $controlRoot.Path; ControlRootCanonical = $controlRoot.Canonical; ControlRootIdentity = $controlRoot.Identity
        SecretPath = $secret.Path; SecretIdentity = $secret.Identity
        Upstream = $upstream; DataPath = $dataPath; DatabasePath = [IO.Path]::Combine($dataPath, 'app.db')
        ResultDirectory = $resultDirectory; ResultPath = $resultPath; SQLiteExecutable = $sqlite
        StartupTimeoutSeconds = $StartupTimeoutSeconds; PreFaultTimeoutSeconds = $PreFaultTimeoutSeconds
        FaultDetectionTimeoutSeconds = $FaultDetectionTimeoutSeconds; RecoveryTimeoutSeconds = $RecoveryTimeoutSeconds
        FinalizationTimeoutSeconds = $FinalizationTimeoutSeconds; CloseTimeoutSeconds = $CloseTimeoutSeconds
        ProbeTimeoutSeconds = $ProbeTimeoutSeconds; PollIntervalMilliseconds = $PollIntervalMilliseconds
        AppLaunchCleanupUncertain = $false; RelayCleanupUncertain = $false
    }
}

function Assert-P3AccProperties {
    param([Parameter(Mandatory)]$Value, [Parameter(Mandatory)][string[]]$Expected)
    if ($null -eq $Value -or $Value -isnot [psobject]) { Throw-P3AccFailure 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' }
    $actual = @($Value.PSObject.Properties | ForEach-Object { $_.Name } | Sort-Object)
    $wanted = @($Expected | Sort-Object)
    if ($actual.Count -ne $wanted.Count -or @(Compare-Object -ReferenceObject $wanted -DifferenceObject $actual).Count -ne 0) {
        Throw-P3AccFailure 'P3ACC_CONTROLLER_SNAPSHOT_INVALID'
    }
}

function Test-P3AccInteger {
    param($Value)
    return $Value -is [byte] -or $Value -is [sbyte] -or $Value -is [int16] -or $Value -is [uint16] -or
        $Value -is [int32] -or $Value -is [uint32] -or $Value -is [int64]
}

function Assert-P3AccNonNegativeInteger {
    param($Value)
    if (-not (Test-P3AccInteger $Value) -or [int64]$Value -lt 0) { Throw-P3AccFailure 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' }
}

function Assert-P3AccBoolean {
    param($Value)
    if ($Value -isnot [bool]) { Throw-P3AccFailure 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' }
}

function Assert-P3AccEnum {
    param($Value, [string[]]$Allowed)
    if ($Value -isnot [string] -or $Allowed -cnotcontains $Value) { Throw-P3AccFailure 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' }
}

function Assert-P3AccMetric {
    param($Metric)
    Assert-P3AccProperties $Metric @('baseline','peak','latest','delta','latterHalfDelta','latterHalfTrend')
    Assert-P3AccNonNegativeInteger $Metric.baseline
    Assert-P3AccNonNegativeInteger $Metric.peak
    Assert-P3AccNonNegativeInteger $Metric.latest
    if (-not (Test-P3AccInteger $Metric.delta) -or -not (Test-P3AccInteger $Metric.latterHalfDelta)) { Throw-P3AccFailure 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' }
    Assert-P3AccEnum $Metric.latterHalfTrend @('INSUFFICIENT','STABLE','RISING','FALLING')
    if ([int64]$Metric.peak -lt [int64]$Metric.baseline -or [int64]$Metric.peak -lt [int64]$Metric.latest -or
        [int64]$Metric.delta -ne ([int64]$Metric.latest - [int64]$Metric.baseline)) {
        Throw-P3AccFailure 'P3ACC_CONTROLLER_SNAPSHOT_INVALID'
    }
}

function Get-P3AccExpectedMetricTrend {
    param([int64]$SampleCount, [int64]$LatterHalfDelta, [int64]$Threshold)
    if ($SampleCount -lt 2) { return 'INSUFFICIENT' }
    if ($LatterHalfDelta -gt $Threshold) { return 'RISING' }
    if ($LatterHalfDelta -lt -$Threshold) { return 'FALLING' }
    return 'STABLE'
}

function Test-P3AccResourceObservationInvariants {
    param($Resources, [switch]$RequireObserved, [switch]$SnapshotDerived)
    try {
        if (-not (Test-P3AccInteger $Resources.sampleCount) -or [int64]$Resources.sampleCount -lt 0 -or
            [int64]$Resources.sampleCount -gt 128) { return $false }
        $sampleCount = [int64]$Resources.sampleCount
        $rateNames = @(
            'averageProcessReadBytesPerSecond','averageProcessWriteBytesPerSecond',
            'latterHalfProcessReadBytesPerSecond','latterHalfProcessWriteBytesPerSecond',
            'averageDiskWriteBytesPerSecond','latterHalfDiskWriteBytesPerSecond'
        )
        foreach ($name in $rateNames) {
            if ($Resources.$name -isnot [ValueType] -or [double]$Resources.$name -lt 0 -or
                [double]::IsNaN([double]$Resources.$name) -or [double]::IsInfinity([double]$Resources.$name)) { return $false }
        }
        $metricThresholds = [ordered]@{
            processCount = 0; workingSet = 1048576; privateBytes = 1048576
            threads = 2; handles = 2; goroutines = 2
            heapAlloc = 1048576; heapInUse = 1048576; system = 1048576
            databaseWalBytes = 1048576
            processReadBytes = 0; processWriteBytes = 0; dataRootPhysicalBytes = 0
            eventQueueCount = 0; eventQueueItems = 8; eventQueueBytes = 65536
            eventQueueItemCapacity = 0; eventQueueByteCapacity = 0
        }
        foreach ($name in $metricThresholds.Keys) {
            $metric = $Resources.$name
            $expectedTrend = Get-P3AccExpectedMetricTrend -SampleCount $sampleCount `
                -LatterHalfDelta ([int64]$metric.latterHalfDelta) -Threshold ([int64]$metricThresholds[$name])
            if ($metric.latterHalfTrend -cne $expectedTrend -or
                ($sampleCount -lt 2 -and [int64]$metric.latterHalfDelta -ne 0)) { return $false }
        }
        $processRead = $Resources.processReadBytes
        $processWrite = $Resources.processWriteBytes
        $physicalWrite = $Resources.dataRootPhysicalBytes
        foreach ($metric in @($processRead,$processWrite,$physicalWrite)) {
            if ([int64]$metric.delta -lt 0 -or [int64]$metric.latterHalfDelta -lt 0 -or
                [int64]$metric.peak -ne [int64]$metric.latest) { return $false }
        }
        $ratePairs = @(
            [pscustomobject]@{ Metric = $processRead; AverageRate = [double]$Resources.averageProcessReadBytesPerSecond; LatterRate = [double]$Resources.latterHalfProcessReadBytesPerSecond },
            [pscustomobject]@{ Metric = $processWrite; AverageRate = [double]$Resources.averageProcessWriteBytesPerSecond; LatterRate = [double]$Resources.latterHalfProcessWriteBytesPerSecond },
            [pscustomobject]@{ Metric = $physicalWrite; AverageRate = [double]$Resources.averageDiskWriteBytesPerSecond; LatterRate = [double]$Resources.latterHalfDiskWriteBytesPerSecond }
        )
        foreach ($pair in $ratePairs) {
            if (([int64]$pair.Metric.delta -gt 0) -ne ($pair.AverageRate -gt 0) -or
                ([int64]$pair.Metric.latterHalfDelta -gt 0) -ne ($pair.LatterRate -gt 0)) { return $false }
        }
        $diskActivity = [int64]$physicalWrite.delta -gt 0 -and [double]$Resources.averageDiskWriteBytesPerSecond -gt 0
        if ([bool]$Resources.diskIoObserved -ne $diskActivity) { return $false }
        $queueActivity = [int64]$Resources.eventQueueCount.peak -gt 0
        if ([bool]$Resources.eventQueueObserved -ne $queueActivity) { return $false }

        $queueCapacityValid = $true
        foreach ($capacity in @($Resources.eventQueueItemCapacity,$Resources.eventQueueByteCapacity)) {
            if ([int64]$capacity.baseline -lt 1 -or [int64]$capacity.latest -lt 1 -or
                [int64]$capacity.baseline -ne [int64]$capacity.peak -or [int64]$capacity.peak -ne [int64]$capacity.latest -or
                [int64]$capacity.delta -ne 0 -or [int64]$capacity.latterHalfDelta -ne 0) { $queueCapacityValid = $false }
        }
        $pairs = @(
            [pscustomobject]@{ Usage = $Resources.eventQueueItems; Capacity = $Resources.eventQueueItemCapacity },
            [pscustomobject]@{ Usage = $Resources.eventQueueBytes; Capacity = $Resources.eventQueueByteCapacity }
        )
        foreach ($pair in $pairs) {
            $usage = $pair.Usage
            $capacity = $pair.Capacity
            if ([int64]$usage.baseline -gt [int64]$capacity.baseline -or
                [int64]$usage.peak -gt [int64]$capacity.baseline -or
                [int64]$usage.latest -gt [int64]$capacity.baseline) { $queueCapacityValid = $false }
        }
        if ($Resources.eventQueueObserved -and -not $queueCapacityValid) { return $false }

        if ($sampleCount -lt 2 -and
            ([bool]$Resources.diskIoObserved -or $Resources.cpuTrend -cne 'INSUFFICIENT')) { return $false }
        if ($SnapshotDerived) {
            foreach ($name in @('sampleComplete','stableWindowProven','cpuWithinTarget')) {
                if ($Resources.$name -isnot [bool]) { return $false }
            }
            if (-not (Test-P3AccInteger $Resources.windowDurationMs) -or [int64]$Resources.windowDurationMs -lt 0 -or
                $Resources.averageCpuPercent -isnot [ValueType] -or [double]$Resources.averageCpuPercent -lt 0 -or
                [double]::IsNaN([double]$Resources.averageCpuPercent) -or [double]::IsInfinity([double]$Resources.averageCpuPercent)) { return $false }
            if ($sampleCount -lt 2) {
                if ($Resources.stableWindowProven -or $Resources.cpuWithinTarget) { return $false }
            } else {
                $eligible = [bool]$Resources.sampleComplete -and $sampleCount -ge 30 -and
                    [int64]$Resources.windowDurationMs -ge 600000 -and
                    [bool]$Resources.databaseWalObserved -and [bool]$Resources.diskIoObserved -and
                    [bool]$Resources.eventQueueObserved -and $queueCapacityValid
                $cpuWithinTarget = $eligible -and [double]$Resources.averageCpuPercent -lt 10
                if ([bool]$Resources.stableWindowProven -ne [bool]$eligible -or
                    [bool]$Resources.cpuWithinTarget -ne [bool]$cpuWithinTarget -or
                    ($eligible -and ([int64]$Resources.eventQueueCount.baseline -lt 1 -or
                        [int64]$Resources.eventQueueCount.latest -lt 1))) { return $false }
            }
        }
        if ($RequireObserved -and
            (-not $Resources.databaseWalObserved -or -not $Resources.diskIoObserved -or -not $Resources.eventQueueObserved)) { return $false }
        return $true
    } catch { return $false }
}

function Assert-P3AccSnapshotContract {
    param([Parameter(Mandatory)]$Snapshot)
    Assert-P3AccProperties $Snapshot @('schema','stage','capturedAt','ui','runtime','progress','database','sessionManifest','mediaManifest','gaps','checkpoint','resources')
    Assert-P3AccEnum $Snapshot.schema @($script:P3AccSnapshotSchema)
    Assert-P3AccEnum $Snapshot.stage @('INITIALIZING','CONFIGURED','WAITING','STARTING','LIVE','RECORDING','RECONNECTING','RECOVERED','FINALIZING','FINALIZED','OFFLINE','ERROR')
    Assert-P3AccNonNegativeInteger $Snapshot.capturedAt

    Assert-P3AccProperties $Snapshot.ui @('ready','recordingSeen','progressAdvanced','timelineSeen','reconnectingSeen','recoveredSeen','networkReconnectingSeen','networkRecoveredSeen','offlineSeen','finalizedSeen','observationCount','latencySampleCount','latencyPendingCount','latencyP95Ms','latencyMaxMs','latencyWithinTarget')
    $uiFlags = @('ready','recordingSeen','progressAdvanced','timelineSeen','reconnectingSeen','recoveredSeen','networkReconnectingSeen','networkRecoveredSeen','offlineSeen','finalizedSeen')
    $uiCount = 0
    foreach ($name in $uiFlags) { Assert-P3AccBoolean $Snapshot.ui.$name; if ($Snapshot.ui.$name) { $uiCount++ } }
    Assert-P3AccNonNegativeInteger $Snapshot.ui.observationCount
    foreach ($name in @('latencySampleCount','latencyPendingCount','latencyP95Ms','latencyMaxMs')) { Assert-P3AccNonNegativeInteger $Snapshot.ui.$name }
    Assert-P3AccBoolean $Snapshot.ui.latencyWithinTarget
    if (([int]$Snapshot.ui.latencySampleCount -eq 0 -and ([int]$Snapshot.ui.latencyP95Ms -ne 0 -or [int]$Snapshot.ui.latencyMaxMs -ne 0)) -or
        ([int]$Snapshot.ui.latencySampleCount -gt 0 -and [int]$Snapshot.ui.latencyP95Ms -gt [int]$Snapshot.ui.latencyMaxMs) -or
        ($Snapshot.ui.latencyWithinTarget -and ([int]$Snapshot.ui.latencySampleCount -lt 1 -or [int]$Snapshot.ui.latencyPendingCount -ne 0 -or [int]$Snapshot.ui.latencyP95Ms -ge 1000))) {
        Throw-P3AccFailure 'P3ACC_CONTROLLER_SNAPSHOT_INVALID'
    }

    if ([int]$Snapshot.ui.observationCount -ne $uiCount -or
        ($Snapshot.ui.recordingSeen -and -not $Snapshot.ui.ready) -or
        ($Snapshot.ui.progressAdvanced -and -not $Snapshot.ui.recordingSeen) -or
        ($Snapshot.ui.timelineSeen -and -not $Snapshot.ui.recordingSeen) -or
        ($Snapshot.ui.reconnectingSeen -and -not $Snapshot.ui.recordingSeen) -or
        ($Snapshot.ui.recoveredSeen -and -not $Snapshot.ui.reconnectingSeen) -or
        ($Snapshot.ui.networkReconnectingSeen -and -not $Snapshot.ui.recoveredSeen) -or
        ($Snapshot.ui.networkRecoveredSeen -and -not $Snapshot.ui.networkReconnectingSeen) -or
        ($Snapshot.ui.offlineSeen -and -not $Snapshot.ui.networkRecoveredSeen) -or
        ($Snapshot.ui.finalizedSeen -and -not $Snapshot.ui.offlineSeen)) { Throw-P3AccFailure 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' }

    Assert-P3AccProperties $Snapshot.runtime @('state','recordingStatus','revision','errorCode','hasSession','sessionFenceStable','currentAttemptCommitted','attemptAdvanced','attemptCount','recorderTargetMatched','crashInjected','recoveryProven','networkFaultArmed','networkRecoveryProven','finalizationProven')
    Assert-P3AccEnum $Snapshot.runtime.state @('STOPPED','WAITING','STARTING','LIVE','RECORDING','RECONNECTING','FINALIZING','ERROR')
    $recordingStates = @('','pending','disabled','starting','recording','unavailable','reconnecting','finalizing','completed','incomplete','failed')
    Assert-P3AccEnum $Snapshot.runtime.recordingStatus $recordingStates
    $errorCodes = @('','UNKNOWN','P3ACC_CONFIG_INVALID','HOOK_CONFIG_INVALID','HOOK_ISOLATION_INVALID','PRIVACY_CONFIG_FAILED','ROOM_CREATE_FAILED','MONITOR_START_FAILED','P3ACC_NOT_READY','P3ACC_SNAPSHOT_FAILED','P3ACC_SNAPSHOT_INVALID','P3ACC_RECORDER_FENCE_MISMATCH','P3ACC_RECORDER_UNAVAILABLE','P3ACC_RECORDER_CRASH_FAILED','ROOM_OFFLINE','ROOM_OFFLINE_CONFIRMING','ROOM_CHECK_FAILED','ROOM_NOT_FOUND','COOKIE_INVALID','CAPTURE_OPEN_FAILED','CAPTURE_REBIND_FAILED','ROOM_CONNECTION_INTERRUPTED','CAPTURE_FINALIZING','CAPTURE_FINALIZE_FAILED','MONITOR_LIMIT_REACHED','MONITOR_MANAGER_SHUTTING_DOWN','FFMPEG_EXITED','FFMPEG_START_FAILED','FFMPEG_STOP_FAILED','FFMPEG_PROGRESS_INVALID','FFMPEG_PROGRESS_STALLED','RECORDING_UNAVAILABLE','RECORDING_RESTARTED','STREAM_UNAVAILABLE','DISK_FULL','PROCESS_CRASH','MESSAGE_DISCONNECT','CLOCK_UNCERTAIN','EVENT_PERSISTENCE','MESSAGE_RECONNECTED','MESSAGE_REBIND_RETRY','MESSAGE_SUBSCRIPTION_FAILED','MESSAGE_REBIND_EXHAUSTED','MESSAGE_FINALIZED','RECORDER_PROCESS_EXITED','RECORDER_NETWORK_FAILURE')
    Assert-P3AccEnum $Snapshot.runtime.errorCode $errorCodes
    Assert-P3AccNonNegativeInteger $Snapshot.runtime.revision
    Assert-P3AccNonNegativeInteger $Snapshot.runtime.attemptCount
    foreach ($name in @('hasSession','sessionFenceStable','currentAttemptCommitted','attemptAdvanced','recorderTargetMatched','crashInjected','recoveryProven','networkFaultArmed','networkRecoveryProven','finalizationProven')) { Assert-P3AccBoolean $Snapshot.runtime.$name }
    if (($Snapshot.runtime.recoveryProven -and (-not $Snapshot.runtime.crashInjected -or -not $Snapshot.runtime.attemptAdvanced)) -or
        ($Snapshot.runtime.networkRecoveryProven -and (-not $Snapshot.runtime.networkFaultArmed -or -not $Snapshot.runtime.recoveryProven)) -or
        ($Snapshot.runtime.finalizationProven -and (-not $Snapshot.runtime.recoveryProven -or -not $Snapshot.runtime.networkRecoveryProven -or $Snapshot.stage -cne 'FINALIZED'))) {
        Throw-P3AccFailure 'P3ACC_CONTROLLER_SNAPSHOT_INVALID'
    }

    Assert-P3AccProperties $Snapshot.progress @('sampleCount','liveBatchCount','liveEventCount','elapsedMs','bytesWritten','segmentCount','restartCount','steadyRecordingMs','steadySampleCount')
    foreach ($name in @('sampleCount','liveBatchCount','liveEventCount','elapsedMs','bytesWritten','segmentCount','restartCount','steadyRecordingMs','steadySampleCount')) { Assert-P3AccNonNegativeInteger $Snapshot.progress.$name }

    Assert-P3AccProperties $Snapshot.database @('sessionCount','activeSessionCount','eventCount','sourceEventCount','publishedEventCount','publishedEventsPersisted','segmentCount','completeSegmentCount','artifactCount','completeArtifactCount')
    foreach ($name in @('sessionCount','activeSessionCount','eventCount','sourceEventCount','publishedEventCount','segmentCount','completeSegmentCount','artifactCount','completeArtifactCount')) { Assert-P3AccNonNegativeInteger $Snapshot.database.$name }
    Assert-P3AccBoolean $Snapshot.database.publishedEventsPersisted

    Assert-P3AccProperties $Snapshot.sessionManifest @('exists','matchesDatabase','canonicalHashMatches','manifestClean','ended','status','recordingStatus')
    foreach ($name in @('exists','matchesDatabase','canonicalHashMatches','manifestClean','ended')) { Assert-P3AccBoolean $Snapshot.sessionManifest.$name }
    Assert-P3AccEnum $Snapshot.sessionManifest.status @('','starting','recording','finalizing','completed','interrupted','failed')
    Assert-P3AccEnum $Snapshot.sessionManifest.recordingStatus $recordingStates
    if ($Snapshot.sessionManifest.matchesDatabase -ne $Snapshot.sessionManifest.canonicalHashMatches -or ($Snapshot.sessionManifest.matchesDatabase -and -not $Snapshot.sessionManifest.exists)) { Throw-P3AccFailure 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' }

    Assert-P3AccProperties $Snapshot.mediaManifest @('exists','matchesDatabase','canonicalHashMatches','manifestClean','state','revision','attemptCount','committedAttemptCount','cleanAttemptCount','segmentCount','completeSegmentCount','artifactCount','completeArtifactCount','fileCheckCount','fileFailureCount','incompleteEntryCount','incompleteSegmentCount','allFilesMatch','sequenceContinuous','attemptReferencesValid','faultPhaseSegmentsProven')
    foreach ($name in @('exists','matchesDatabase','canonicalHashMatches','manifestClean','allFilesMatch','sequenceContinuous','attemptReferencesValid','faultPhaseSegmentsProven')) { Assert-P3AccBoolean $Snapshot.mediaManifest.$name }
    Assert-P3AccEnum $Snapshot.mediaManifest.state @('','open','finalizing','completed','incomplete')
    foreach ($name in @('revision','attemptCount','committedAttemptCount','cleanAttemptCount','segmentCount','completeSegmentCount','artifactCount','completeArtifactCount','fileCheckCount','fileFailureCount','incompleteEntryCount','incompleteSegmentCount')) { Assert-P3AccNonNegativeInteger $Snapshot.mediaManifest.$name }
    if ($Snapshot.mediaManifest.matchesDatabase -ne $Snapshot.mediaManifest.canonicalHashMatches -or
        ($Snapshot.mediaManifest.matchesDatabase -and -not $Snapshot.mediaManifest.exists) -or
        [int]$Snapshot.mediaManifest.fileFailureCount -gt [int]$Snapshot.mediaManifest.fileCheckCount -or
        $Snapshot.mediaManifest.allFilesMatch -ne ([int]$Snapshot.mediaManifest.fileCheckCount -gt 0 -and [int]$Snapshot.mediaManifest.fileFailureCount -eq 0)) { Throw-P3AccFailure 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' }

    Assert-P3AccProperties $Snapshot.gaps @('total','open','recovered','recordingRestart','openRecordingRestart','openMessageDisconnect','processCrash','messageDisconnect','crashRecoveryMatched','networkMessageMatched','networkRecorderMatched','latestKind','latestReasonCode','latestOpen','latestRecovered')
    foreach ($name in @('total','open','recovered','recordingRestart','openRecordingRestart','openMessageDisconnect','processCrash','messageDisconnect')) { Assert-P3AccNonNegativeInteger $Snapshot.gaps.$name }
    foreach ($name in @('crashRecoveryMatched','networkMessageMatched','networkRecorderMatched','latestOpen','latestRecovered')) { Assert-P3AccBoolean $Snapshot.gaps.$name }
    Assert-P3AccEnum $Snapshot.gaps.latestKind @('','message_disconnect','recording_restart','stream_unavailable','disk_full','process_crash','clock_uncertain','event_persistence')
    Assert-P3AccEnum $Snapshot.gaps.latestReasonCode $errorCodes

    Assert-P3AccProperties $Snapshot.checkpoint @('exists','state','committedSequence','maxSourceSequence','coversSourceEvents','openGiftFoldCount','giftFoldsClosed')
    Assert-P3AccBoolean $Snapshot.checkpoint.exists
    Assert-P3AccEnum $Snapshot.checkpoint.state @('','open','closing','closed','degraded')
    foreach ($name in @('committedSequence','maxSourceSequence','openGiftFoldCount')) { Assert-P3AccNonNegativeInteger $Snapshot.checkpoint.$name }
    foreach ($name in @('coversSourceEvents','giftFoldsClosed')) { Assert-P3AccBoolean $Snapshot.checkpoint.$name }

    Assert-P3AccProperties $Snapshot.resources @('sampleCount','windowDurationMs','sampleComplete','stableWindowProven','frozen','averageCpuPercent','latterHalfAverageCpuPercent','cpuWithinTarget','cpuTrend','databaseWalObserved','diskIoObserved','eventQueueObserved','averageProcessReadBytesPerSecond','averageProcessWriteBytesPerSecond','latterHalfProcessReadBytesPerSecond','latterHalfProcessWriteBytesPerSecond','averageDiskWriteBytesPerSecond','latterHalfDiskWriteBytesPerSecond','processCount','workingSet','privateBytes','threads','handles','goroutines','heapAlloc','heapInUse','system','databaseWalBytes','processReadBytes','processWriteBytes','dataRootPhysicalBytes','eventQueueCount','eventQueueItems','eventQueueBytes','eventQueueItemCapacity','eventQueueByteCapacity')
    Assert-P3AccNonNegativeInteger $Snapshot.resources.sampleCount
    Assert-P3AccNonNegativeInteger $Snapshot.resources.windowDurationMs
    foreach ($name in @('sampleComplete','stableWindowProven','frozen','cpuWithinTarget','databaseWalObserved','diskIoObserved','eventQueueObserved')) { Assert-P3AccBoolean $Snapshot.resources.$name }
    foreach ($name in @('averageCpuPercent','latterHalfAverageCpuPercent','averageProcessReadBytesPerSecond','averageProcessWriteBytesPerSecond','latterHalfProcessReadBytesPerSecond','latterHalfProcessWriteBytesPerSecond','averageDiskWriteBytesPerSecond','latterHalfDiskWriteBytesPerSecond')) {
        if ($Snapshot.resources.$name -isnot [ValueType] -or [double]$Snapshot.resources.$name -lt 0 -or [double]::IsNaN([double]$Snapshot.resources.$name) -or [double]::IsInfinity([double]$Snapshot.resources.$name)) { Throw-P3AccFailure 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' }
    }
    Assert-P3AccEnum $Snapshot.resources.cpuTrend @('INSUFFICIENT','STABLE','RISING','FALLING')
    foreach ($name in @('processCount','workingSet','privateBytes','threads','handles','goroutines','heapAlloc','heapInUse','system','databaseWalBytes','processReadBytes','processWriteBytes','dataRootPhysicalBytes','eventQueueCount','eventQueueItems','eventQueueBytes','eventQueueItemCapacity','eventQueueByteCapacity')) {
        Assert-P3AccMetric $Snapshot.resources.$name
    }
    if (-not (Test-P3AccResourceObservationInvariants $Snapshot.resources -SnapshotDerived)) {
        Throw-P3AccFailure 'P3ACC_CONTROLLER_SNAPSHOT_INVALID'
    }
    if ([int]$Snapshot.resources.sampleCount -gt 128) { Throw-P3AccFailure 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' }
    return $true
}

function Read-P3AccSnapshot {
    param([Parameter(Mandatory)]$Configuration)
    Assert-P3AccRootStillOwned $Configuration
    if (-not (Test-Path -LiteralPath $Configuration.ResultPath -PathType Leaf)) { return $null }
    try {
        $file = [IO.File]::Open($Configuration.ResultPath, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::ReadWrite -bor [IO.FileShare]::Delete)
        try {
            if ($file.Length -lt 2 -or $file.Length -gt $script:P3AccMaximumSnapshotBytes) { Throw-P3AccFailure 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' }
            $bytes = [byte[]]::new([int]$file.Length)
            $offset = 0
            while ($offset -lt $bytes.Length) {
                $read = $file.Read($bytes, $offset, $bytes.Length - $offset)
                if ($read -le 0) { Throw-P3AccFailure 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' }
                $offset += $read
            }
        } finally { $file.Dispose() }
        $utf8 = [Text.UTF8Encoding]::new($false, $true)
        $json = $utf8.GetString($bytes)
        $snapshot = $json | ConvertFrom-Json -ErrorAction Stop
        $json = $null; $bytes = $null
    } catch {
        if ($_.Exception.Message -ceq 'P3ACC_CONTROLLER_SNAPSHOT_INVALID') { throw }
        Throw-P3AccFailure 'P3ACC_CONTROLLER_SNAPSHOT_INVALID'
    }
    [void](Assert-P3AccSnapshotContract $snapshot)
    $nowMs = [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds()
    if ([int64]$snapshot.capturedAt -gt $nowMs + 30000 -or $nowMs - [int64]$snapshot.capturedAt -gt 120000) { Throw-P3AccFailure 'P3ACC_CONTROLLER_SNAPSHOT_INVALID' }
    return $snapshot
}

function Read-P3AccRelayAnnouncementText {
    param($Raw)
    if ($null -eq $Raw) { return $null }
    $text = [string]$Raw
    if ($text -notmatch '^([1-9][0-9]{0,4})\r?\n$') { Throw-P3AccFailure 'P3ACC_CONTROLLER_RELAY_FAILED' }
    $port = [int]$Matches[1]
    if ($port -lt 1 -or $port -gt 65535) { Throw-P3AccFailure 'P3ACC_CONTROLLER_RELAY_FAILED' }
    return $port
}

function Test-P3AccProcessIdentity {
    param($Identity)
    if ($null -eq $Identity) { return $false }
    $process = $null
    try {
        $process = [Diagnostics.Process]::GetProcessById([int]$Identity.ProcessId)
        if ($process.HasExited) { return $false }
        $ticks = $process.StartTime.ToUniversalTime().Ticks
        if ($process.HasExited) { return $false }
        return $ticks -eq [int64]$Identity.StartedAtUtcTicks
    } catch { return $false }
    finally {
        if ($null -ne $process) { try { $process.Dispose() } catch { } }
    }
}

function Start-P3AccRelay {
    param([Parameter(Mandatory)]$Configuration, [Parameter(Mandatory)][int]$ListenPort, [Parameter(Mandatory)][string]$Phase)
    Assert-P3AccRootStillOwned $Configuration
    $announcePath = [IO.Path]::Combine($Configuration.ControlRoot, "relay-$Phase.port")
    if (Test-Path -LiteralPath $announcePath) { Throw-P3AccFailure 'P3ACC_CONTROLLER_RELAY_FAILED' }
    [void](Assert-P3AccControlRootStillOwned $Configuration)
    $listen = "127.0.0.1:$ListenPort"
    $process = $null
    $identity = $null
    $succeeded = $false
    try {
        $process = Start-Process -FilePath $Configuration.RelayExecutable -ArgumentList @('-listen', $listen, '-upstream', $Configuration.Upstream.Canonical, '-announce-port') -RedirectStandardOutput $announcePath -WindowStyle Hidden -PassThru -ErrorAction Stop
        $identity = [pscustomobject]@{ ProcessId = $process.Id; StartedAtUtcTicks = $process.StartTime.ToUniversalTime().Ticks }
        $deadline = [DateTime]::UtcNow.AddSeconds($Configuration.StartupTimeoutSeconds)
        $port = $null
        while ([DateTime]::UtcNow -lt $deadline) {
            if (-not (Test-P3AccProcessIdentity $identity)) { Throw-P3AccFailure 'P3ACC_CONTROLLER_RELAY_FAILED' }
            if (Test-Path -LiteralPath $announcePath -PathType Leaf) {
                $raw = Get-Content -LiteralPath $announcePath -Raw -ErrorAction Stop
                if ($null -ne $raw -and ([string]$raw).Length -gt 0) {
                    $port = Read-P3AccRelayAnnouncementText $raw
                    break
                }
            }
            Start-Sleep -Milliseconds 100
        }
        if ($null -eq $port -or ($ListenPort -ne 0 -and $port -ne $ListenPort)) { Throw-P3AccFailure 'P3ACC_CONTROLLER_RELAY_FAILED' }
        $state = [pscustomobject]@{ Process = $process; Identity = $identity; Port = [int]$port; AnnouncementPath = $announcePath }
        $succeeded = $true
        return $state
    } catch {
        if ($_.Exception.Message -ceq 'P3ACC_CONTROLLER_RELAY_FAILED') { throw }
        Throw-P3AccFailure 'P3ACC_CONTROLLER_RELAY_FAILED'
    } finally {
        $cleanupFailed = $false
        if (-not $succeeded -and $null -ne $identity) {
            $state = [pscustomobject]@{ Process = $process; Identity = $identity }
            $stopped = Stop-P3AccExactProcess $state
            try { $process.Dispose() } catch { }
            if (-not $stopped) { $cleanupFailed = $true }
        } elseif (-not $succeeded -and $null -ne $process) {
            $stopped = $false
            try {
                $process.Refresh()
                if (-not $process.HasExited) { $process.Kill() }
                $stopped = $process.WaitForExit(10000)
            } catch {
                try { $process.Refresh(); $stopped = $process.HasExited } catch { $stopped = $false }
            } finally {
                try { $process.Dispose() } catch { }
            }
            if (-not $stopped) { $cleanupFailed = $true }
        }
        if (-not $succeeded) {
            try {
                [void](Assert-P3AccControlRootStillOwned $Configuration)
                if (Test-Path -LiteralPath $announcePath) {
                    [void](Assert-P3AccNoReparsePath -Path $announcePath -Directory $false)
                    Remove-Item -LiteralPath $announcePath -Force -ErrorAction Stop
                    if (Test-Path -LiteralPath $announcePath) { throw 'announce cleanup failed' }
                }
            } catch { $cleanupFailed = $true }
            if ($cleanupFailed) { $Configuration.RelayCleanupUncertain = $true; Throw-P3AccFailure 'P3ACC_CONTROLLER_CLEANUP_FAILED' }
        }
    }
}

function Stop-P3AccExactProcess {
    param($ProcessState, [int]$WaitSeconds = 10)
    if ($null -eq $ProcessState) { return $true }
    if (-not (Test-P3AccProcessIdentity $ProcessState.Identity)) { return $true }
    try {
        $ProcessState.Process.Refresh()
        $ProcessState.Process.Kill()
        return $ProcessState.Process.WaitForExit($WaitSeconds * 1000) -and -not (Test-P3AccProcessIdentity $ProcessState.Identity)
    } catch { return -not (Test-P3AccProcessIdentity $ProcessState.Identity) }
}

function Test-P3AccRelayProbe {
    param([int]$Port, [int]$TimeoutSeconds)
    $oldProtocol = [Net.ServicePointManager]::SecurityProtocol
    try {
        [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
        $request = [Net.HttpWebRequest]::Create($script:P3AccProbeUri)
        $request.Method = 'GET'
        $request.AllowAutoRedirect = $false
        $request.Timeout = $TimeoutSeconds * 1000
        $request.ReadWriteTimeout = $TimeoutSeconds * 1000
        $request.Proxy = [Net.WebProxy]::new("http://127.0.0.1:$Port", $false)
        $request.UserAgent = 'P3ACC-Controller'
        try {
            $response = [Net.HttpWebResponse]$request.GetResponse()
            try { $status = [int]$response.StatusCode }
            finally { $response.Close() }
            return $status -ge 200 -and $status -lt 500
        } catch [Net.WebException] {
            if ($null -ne $_.Exception.Response) {
                $response = [Net.HttpWebResponse]$_.Exception.Response
                try { $status = [int]$response.StatusCode }
                finally { $response.Close() }
                return $status -ge 200 -and $status -lt 500
            }
            return $false
        }
    } finally { [Net.ServicePointManager]::SecurityProtocol = $oldProtocol }
}

function ConvertTo-P3AccSingleQuotedLiteral {
    param([string]$Value)
    return "'" + $Value.Replace("'", "''") + "'"
}

function ConvertTo-P3AccWindowsCommandLineArgument {
    param([Parameter(Mandatory)][AllowEmptyString()][string]$Value)
    if ($Value.Length -gt 30000 -or $Value.IndexOf([char]0) -ge 0 -or $Value.IndexOf([char]13) -ge 0 -or $Value.IndexOf([char]10) -ge 0) {
        Throw-P3AccFailure 'P3ACC_CONTROLLER_CONFIG_INVALID'
    }
    if ($Value.Length -gt 0 -and $Value -cnotmatch '[\s"]') { return $Value }
    $builder = [Text.StringBuilder]::new($Value.Length + 8)
    [void]$builder.Append([char]34)
    $slashes = 0
    foreach ($character in $Value.ToCharArray()) {
        if ($character -eq [char]92) { $slashes++; continue }
        if ($character -eq [char]34) {
            if ($slashes -gt 0) { [void]$builder.Append([char]92, (2 * $slashes)) }
            [void]$builder.Append([char]92)
            [void]$builder.Append([char]34)
            $slashes = 0
            continue
        }
        if ($slashes -gt 0) { [void]$builder.Append([char]92, $slashes); $slashes = 0 }
        [void]$builder.Append($character)
    }
    if ($slashes -gt 0) { [void]$builder.Append([char]92, (2 * $slashes)) }
    [void]$builder.Append([char]34)
    return $builder.ToString()
}

function New-P3AccAppJobState {
    param([string]$Nonce)
    if ([string]::IsNullOrWhiteSpace($Nonce)) { $Nonce = [Guid]::NewGuid().ToString('N') }
    if ($Nonce -cnotmatch '^[0-9a-f]{32}$') { Throw-P3AccFailure 'P3ACC_CONTROLLER_APP_LAUNCH_FAILED' }
    $name = 'Global\DouyinLive.P3ACC.App.' + $Nonce
    $handle = $null
    try {
        $sid = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
        $handle = [P3Acc.NativeJob]::CreateOwned($name, $sid)
        if ($null -eq $handle -or $handle.IsInvalid -or $handle.IsClosed -or
            [P3Acc.NativeJob]::GetLimitFlags($handle) -ne 0x00002000 -or
            -not [P3Acc.NativeJob]::HasExactProtectedDacl($handle, $sid)) {
            throw 'P3ACC_JOB_INVALID'
        }
        return [pscustomobject]@{
            Name = $name; Nonce = $Nonce; Handle = $handle; Closed = $false
            GracefulExitConfirmed = $false; ForcedEmptyConfirmed = $false
        }
    } catch {
        if ($null -ne $handle) { try { $handle.Dispose() } catch { } }
        Throw-P3AccFailure 'P3ACC_CONTROLLER_APP_LAUNCH_FAILED'
    }
}

function Test-P3AccJobStateOpen {
    param($JobState)
    return $null -ne $JobState -and $JobState.PSObject.Properties.Name -ccontains 'Handle' -and
        $JobState.PSObject.Properties.Name -ccontains 'Closed' -and -not [bool]$JobState.Closed -and
        $null -ne $JobState.Handle -and -not $JobState.Handle.IsClosed -and -not $JobState.Handle.IsInvalid
}

function Get-P3AccJobActiveProcessCount {
    param([Parameter(Mandatory)]$JobState)
    if (-not (Test-P3AccJobStateOpen $JobState)) { throw 'P3ACC_JOB_INVALID' }
    return [uint32][P3Acc.NativeJob]::GetActiveProcesses($JobState.Handle)
}

function Get-P3AccJobIdentityState {
    param([Parameter(Mandatory)]$JobState, [Parameter(Mandatory)]$Identity)
    if (-not (Test-P3AccJobStateOpen $JobState) -or $null -eq $Identity -or
        $Identity.PSObject.Properties.Name -cnotcontains 'ProcessId' -or
        $Identity.PSObject.Properties.Name -cnotcontains 'StartedAtUtcTicks') { return 'Invalid' }
    $process = $null
    try {
        $process = [Diagnostics.Process]::GetProcessById([int]$Identity.ProcessId)
        if ($process.HasExited) { return 'Exited' }
        $ticks = $process.StartTime.ToUniversalTime().Ticks
        if ($process.HasExited) { return 'Exited' }
        if ($ticks -ne [int64]$Identity.StartedAtUtcTicks) { return 'Reused' }
        if ([P3Acc.NativeJob]::ContainsProcess($JobState.Handle, $process.Handle)) { return 'Member' }
        return 'Outside'
    } catch {
        if ((Test-P3AccExceptionChainType $_ ([ArgumentException])) -or
            (Test-P3AccExceptionChainType $_ ([InvalidOperationException]))) { return 'Exited' }
        return 'Invalid'
    } finally {
        if ($null -ne $process) { try { $process.Dispose() } catch { } }
    }
}

function Wait-P3AccJobEmpty {
    param(
        [Parameter(Mandatory)]$JobState,
        [int]$TimeoutSeconds = 10,
        [ValidateRange(1, 2)][int]$ConsecutiveZeroSamples = 2,
        [ValidateRange(50, 1000)][int]$PollMilliseconds = 100
    )
    if ($TimeoutSeconds -lt 1 -or $TimeoutSeconds -gt 120 -or -not (Test-P3AccJobStateOpen $JobState)) { return $false }
    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    $zeroSamples = 0
    while ([DateTime]::UtcNow -lt $deadline) {
        try { $active = Get-P3AccJobActiveProcessCount $JobState }
        catch { return $false }
        if ($active -eq 0) { $zeroSamples++ } else { $zeroSamples = 0 }
        if ($zeroSamples -ge $ConsecutiveZeroSamples) { return $true }
        Start-Sleep -Milliseconds $PollMilliseconds
    }
    return $false
}

function Close-P3AccJobState {
    param([Parameter(Mandatory)]$JobState)
    if ($JobState.PSObject.Properties.Name -cnotcontains 'Closed') { return $false }
    if ([bool]$JobState.Closed) { return $true }
    if (-not (Test-P3AccJobStateOpen $JobState)) { return $false }
    try {
        $JobState.Handle.Dispose()
        $JobState.Closed = $true
        return $true
    } catch { return $false }
}

function Stop-P3AccLauncherOutsideJob {
    param($JobState, $LauncherIdentity)
    if ($null -eq $LauncherIdentity) { return $true }
    $state = Get-P3AccJobIdentityState -JobState $JobState -Identity $LauncherIdentity
    if ($state -ceq 'Exited' -or $state -ceq 'Member') { return $true }
    if ($state -cne 'Outside') { return $false }
    return Stop-P3AccIdentity -Identity $LauncherIdentity -WaitSeconds 10
}

function Stop-P3AccJobState {
    param([Parameter(Mandatory)]$JobState, [int]$TimeoutSeconds = 30)
    if (-not (Test-P3AccJobStateOpen $JobState)) {
        return [bool]$JobState.Closed -and ([bool]$JobState.GracefulExitConfirmed -or [bool]$JobState.ForcedEmptyConfirmed)
    }
    try {
        [P3Acc.NativeJob]::Terminate($JobState.Handle, [uint32]3758096385)
        if (-not (Wait-P3AccJobEmpty -JobState $JobState -TimeoutSeconds $TimeoutSeconds -ConsecutiveZeroSamples 1)) { return $false }
        $JobState.ForcedEmptyConfirmed = $true
        return Close-P3AccJobState $JobState
    } catch { return $false }
}

function Write-P3AccLaunchScript {
    param(
        [Parameter(Mandatory)]$Configuration, [Parameter(Mandatory)][int]$RelayPort,
        [Parameter(Mandatory)][string]$LaunchPath, [Parameter(Mandatory)][string]$PreIdentityPath,
        [Parameter(Mandatory)][string]$HandshakePath, [Parameter(Mandatory)]$JobState
    )
    $launcher = ConvertTo-P3AccSingleQuotedLiteral $Configuration.LauncherExecutable
    $working = ConvertTo-P3AccSingleQuotedLiteral ([IO.Path]::GetDirectoryName($Configuration.AppExecutable))
    $secret = ConvertTo-P3AccSingleQuotedLiteral $Configuration.SecretPath
    $root = ConvertTo-P3AccSingleQuotedLiteral $Configuration.Root
    $module = ConvertTo-P3AccSingleQuotedLiteral $PSCommandPath
    $secretIdentity = ConvertTo-P3AccSingleQuotedLiteral $Configuration.SecretIdentity
    $result = ConvertTo-P3AccSingleQuotedLiteral $Configuration.ResultPath
    $preIdentity = ConvertTo-P3AccSingleQuotedLiteral $PreIdentityPath
    $jobNonce = ConvertTo-P3AccSingleQuotedLiteral $JobState.Nonce
    $proxy = ConvertTo-P3AccSingleQuotedLiteral "http://127.0.0.1:$RelayPort"
    $argumentValues = @(
        '--job-name', [string]$JobState.Name, '--job-nonce', [string]$JobState.Nonce,
        '--app', [string]$Configuration.AppExecutable,
        '--working-dir', [IO.Path]::GetDirectoryName($Configuration.AppExecutable),
        '--handshake', $HandshakePath
    )
    $launcherArguments = ConvertTo-P3AccSingleQuotedLiteral (($argumentValues | ForEach-Object { ConvertTo-P3AccWindowsCommandLineArgument ([string]$_) }) -join ' ')
    $content = @"
Set-StrictMode -Version Latest
`$ErrorActionPreference = 'Stop'
Import-Module -Name $module -Force -ErrorAction Stop
`$process = `$null
`$handedOff = `$false
try {
    `$secretPath = $secret
    `$secretNow = Assert-P3AccSecretFile -Path `$secretPath -Root $root
    if (`$secretNow.Identity -cne $secretIdentity) { throw 'P3ACC_LAUNCH_INVALID' }
    `$stream = [IO.FileStream]::new(`$secretPath, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::Read, 4096, [IO.FileOptions]::DeleteOnClose)
    try {
        `$opened = [P3Acc.NativePath]::InspectHandle(`$stream.SafeFileHandle, `$false)
        if (`$opened.Key -cne $secretIdentity -or `$stream.Length -lt 1 -or `$stream.Length -gt 8192) { throw 'P3ACC_LAUNCH_INVALID' }
        `$bytes = [byte[]]::new([int]`$stream.Length)
        `$offset = 0
        while (`$offset -lt `$bytes.Length) {
            `$read = `$stream.Read(`$bytes, `$offset, `$bytes.Length - `$offset)
            if (`$read -le 0) { throw 'P3ACC_LAUNCH_INVALID' }
            `$offset += `$read
        }
        `$utf8 = [Text.UTF8Encoding]::new(`$false, `$true)
        `$liveUrl = `$utf8.GetString(`$bytes)
        `$bytes = `$null
    } finally {
        `$stream.Dispose()
    }
    if (Test-Path -LiteralPath `$secretPath) { throw 'P3ACC_LAUNCH_INVALID' }
    `$env:HTTP_PROXY = $proxy
    `$env:HTTPS_PROXY = $proxy
    `$env:http_proxy = $proxy
    `$env:https_proxy = $proxy
    Remove-Item Env:NO_PROXY -ErrorAction SilentlyContinue
    Remove-Item Env:no_proxy -ErrorAction SilentlyContinue
    `$env:P3ACC_ROOT = $root
    `$env:P3ACC_RESULT_PATH = $result
    `$env:P3ACC_LIVE_URL = `$liveUrl
    `$process = Start-Process -FilePath $launcher -ArgumentList $launcherArguments -WorkingDirectory $working -PassThru -ErrorAction Stop
    `$started = `$process.StartTime.ToUniversalTime().Ticks
    `$payload = [ordered]@{ schema = 'P3ACC-LAUNCHER-IDENTITY/v1'; jobNonce = $jobNonce; launcherProcessId = `$process.Id; launcherStartedAtUtcTicks = `$started } | ConvertTo-Json -Compress
    `$temporary = $preIdentity + '.tmp'
    [IO.File]::WriteAllText(`$temporary, `$payload, [Text.UTF8Encoding]::new(`$false))
    Move-Item -LiteralPath `$temporary -Destination $preIdentity -ErrorAction Stop
    `$handedOff = `$true
} catch {
    if (`$null -ne `$process) {
        try {
            `$process.Refresh()
            if (-not `$process.HasExited) { `$process.Kill() }
            if (-not `$process.WaitForExit(10000)) { throw 'P3ACC_LAUNCH_CLEANUP_FAILED' }
        } catch { throw 'P3ACC_LAUNCH_CLEANUP_FAILED' }
    }
    Remove-Item -LiteralPath ($preIdentity + '.tmp') -Force -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath $preIdentity -Force -ErrorAction SilentlyContinue
    throw
} finally {
    Remove-Item Env:P3ACC_LIVE_URL,Env:P3ACC_ROOT,Env:P3ACC_RESULT_PATH -ErrorAction SilentlyContinue
    Remove-Item Env:HTTP_PROXY,Env:HTTPS_PROXY,Env:http_proxy,Env:https_proxy -ErrorAction SilentlyContinue
    `$liveUrl = `$null
    `$bytes = `$null
    if (-not `$handedOff -and `$null -ne `$process) { try { `$process.Dispose() } catch { } }
}
if (-not `$handedOff) { throw 'P3ACC_LAUNCH_INVALID' }
"@
    [IO.File]::WriteAllText($LaunchPath, $content, [Text.UTF8Encoding]::new($false))
}

function Read-P3AccLauncherDocument {
    param([Parameter(Mandatory)]$Configuration, [Parameter(Mandatory)][string]$Path)
    $stream = $null
    try {
        [void](Assert-P3AccControlRootStillOwned $Configuration)
        $pathInfo = Assert-P3AccNoReparsePath -Path $Path -Directory $false
        $stream = [IO.FileStream]::new($pathInfo.Path, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::Read, 4096, [IO.FileOptions]::SequentialScan)
        $opened = [P3Acc.NativePath]::InspectHandle($stream.SafeFileHandle, $false)
        if ($opened.Key -cne $pathInfo.Identity -or $stream.Length -lt 2 -or $stream.Length -gt 8192) { throw 'invalid' }
        $bytes = [byte[]]::new([int]$stream.Length)
        $offset = 0
        while ($offset -lt $bytes.Length) {
            $read = $stream.Read($bytes, $offset, $bytes.Length - $offset)
            if ($read -le 0) { throw 'invalid' }
            $offset += $read
        }
        $text = [Text.UTF8Encoding]::new($false, $true).GetString($bytes)
        return $text | ConvertFrom-Json -ErrorAction Stop
    } catch {
        Throw-P3AccFailure 'P3ACC_CONTROLLER_APP_LAUNCH_FAILED'
    } finally {
        if ($null -ne $stream) { try { $stream.Dispose() } catch { } }
    }
}

function ConvertFrom-P3AccLauncherIdentityDocument {
    param([Parameter(Mandatory)]$Document, [Parameter(Mandatory)][string]$JobNonce)
    Assert-P3AccProperties $Document @('schema','jobNonce','launcherProcessId','launcherStartedAtUtcTicks')
    if ($Document.schema -cne 'P3ACC-LAUNCHER-IDENTITY/v1' -or $Document.jobNonce -cne $JobNonce -or
        -not (Test-P3AccInteger $Document.launcherProcessId) -or [int64]$Document.launcherProcessId -lt 1 -or [int64]$Document.launcherProcessId -gt [int]::MaxValue -or
        -not (Test-P3AccInteger $Document.launcherStartedAtUtcTicks) -or [int64]$Document.launcherStartedAtUtcTicks -lt 1) {
        Throw-P3AccFailure 'P3ACC_CONTROLLER_APP_LAUNCH_FAILED'
    }
    return [pscustomobject]@{ ProcessId = [int]$Document.launcherProcessId; StartedAtUtcTicks = [int64]$Document.launcherStartedAtUtcTicks }
}

function Wait-P3AccLauncherExitAfterHandshake {
    param([Parameter(Mandatory)]$JobState, [Parameter(Mandatory)]$LauncherIdentity, [int]$TimeoutSeconds = 10)
    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    while ([DateTime]::UtcNow -lt $deadline) {
        $state = Get-P3AccJobIdentityState -JobState $JobState -Identity $LauncherIdentity
        if ($state -ceq 'Exited') { return $true }
        if ($state -cne 'Member') { return $false }
        Start-Sleep -Milliseconds 50
    }
    return $false
}

function Start-P3AccInteractiveApp {
    param([Parameter(Mandatory)]$Configuration, [Parameter(Mandatory)][int]$RelayPort)
    Assert-P3AccRootStillOwned $Configuration
    $launchPath = [IO.Path]::Combine($Configuration.ControlRoot, 'launch.ps1')
    [void](Assert-P3AccControlRootStillOwned $Configuration)
    $preIdentityPath = [IO.Path]::Combine($Configuration.ControlRoot, 'launcher.identity.json')
    $handshakePath = [IO.Path]::Combine($Configuration.ControlRoot, 'launcher.handshake.json')
    $taskName = 'DouyinLive.P3ACC.' + [Guid]::NewGuid().ToString('N')
    $arguments = '-NoProfile -NonInteractive -WindowStyle Hidden -ExecutionPolicy Bypass -File "' + $launchPath + '"'
    $identity = $null
    $launcherIdentity = $null
    $process = $null
    $jobState = $null
    $succeeded = $false
    $taskStarted = $false
    try {
        foreach ($path in @($launchPath, $preIdentityPath, ($preIdentityPath + '.tmp'), $handshakePath, ($handshakePath + '.tmp'))) {
            if (Test-Path -LiteralPath $path) { Throw-P3AccFailure 'P3ACC_CONTROLLER_APP_LAUNCH_FAILED' }
        }
        $jobState = New-P3AccAppJobState
        Write-P3AccLaunchScript -Configuration $Configuration -RelayPort $RelayPort -LaunchPath $launchPath -PreIdentityPath $preIdentityPath -HandshakePath $handshakePath -JobState $jobState
        $action = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument $arguments
        $principal = New-ScheduledTaskPrincipal -UserId ([Security.Principal.WindowsIdentity]::GetCurrent().Name) -LogonType Interactive -RunLevel Limited
        $settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit (New-TimeSpan -Minutes 5) -MultipleInstances IgnoreNew
        Register-ScheduledTask -TaskName $taskName -Action $action -Principal $principal -Settings $settings -Force | Out-Null
        $secretNow = Assert-P3AccSecretFile -Path $Configuration.SecretPath -Root $Configuration.Root
        if ($secretNow.Identity -cne $Configuration.SecretIdentity) { Throw-P3AccFailure 'P3ACC_CONTROLLER_SECRET_INVALID' }
        Start-ScheduledTask -TaskName $taskName
        $taskStarted = $true
        $deadline = [DateTime]::UtcNow.AddSeconds($Configuration.StartupTimeoutSeconds)
        while ([DateTime]::UtcNow -lt $deadline) {
            if ($null -eq $launcherIdentity -and (Test-Path -LiteralPath $preIdentityPath -PathType Leaf)) {
                $preDocument = Read-P3AccLauncherDocument -Configuration $Configuration -Path $preIdentityPath
                $launcherIdentity = ConvertFrom-P3AccLauncherIdentityDocument -Document $preDocument -JobNonce $jobState.Nonce
            }
            if ($null -ne $launcherIdentity -and (Test-Path -LiteralPath $handshakePath -PathType Leaf)) {
                $handshake = Read-P3AccLauncherDocument -Configuration $Configuration -Path $handshakePath
                Assert-P3AccProperties $handshake @('schema','jobNonce','launcherProcessId','launcherStartedAtUtcTicks','appProcessId','appStartedAtUtcTicks')
                if ($handshake.schema -cne 'P3ACC-LAUNCHER/v1' -or $handshake.jobNonce -cne $jobState.Nonce -or
                    -not (Test-P3AccInteger $handshake.launcherProcessId) -or [int64]$handshake.launcherProcessId -ne [int]$launcherIdentity.ProcessId -or
                    -not (Test-P3AccInteger $handshake.launcherStartedAtUtcTicks) -or [int64]$handshake.launcherStartedAtUtcTicks -ne [int64]$launcherIdentity.StartedAtUtcTicks -or
                    -not (Test-P3AccInteger $handshake.appProcessId) -or [int64]$handshake.appProcessId -lt 1 -or [int64]$handshake.appProcessId -gt [int]::MaxValue -or
                    -not (Test-P3AccInteger $handshake.appStartedAtUtcTicks) -or [int64]$handshake.appStartedAtUtcTicks -lt 1) {
                    Throw-P3AccFailure 'P3ACC_CONTROLLER_APP_LAUNCH_FAILED'
                }
                $identity = [pscustomobject]@{ ProcessId = [int]$handshake.appProcessId; StartedAtUtcTicks = [int64]$handshake.appStartedAtUtcTicks }
                if ((Get-P3AccJobIdentityState -JobState $jobState -Identity $identity) -cne 'Member' -or
                    (Get-P3AccJobActiveProcessCount $jobState) -lt 1 -or
                    -not (Wait-P3AccLauncherExitAfterHandshake -JobState $jobState -LauncherIdentity $launcherIdentity -TimeoutSeconds 10) -or
                    (Get-P3AccJobIdentityState -JobState $jobState -Identity $identity) -cne 'Member' -or
                    (Get-P3AccJobActiveProcessCount $jobState) -lt 1) {
                    Throw-P3AccFailure 'P3ACC_CONTROLLER_APP_LAUNCH_FAILED'
                }
                break
            }
            Start-Sleep -Milliseconds 100
        }
        if ($null -eq $launcherIdentity -or $null -eq $identity -or (Test-Path -LiteralPath $Configuration.SecretPath)) { Throw-P3AccFailure 'P3ACC_CONTROLLER_APP_LAUNCH_FAILED' }
        $process = [Diagnostics.Process]::GetProcessById($identity.ProcessId)
        if ($process.HasExited -or $process.StartTime.ToUniversalTime().Ticks -ne [int64]$identity.StartedAtUtcTicks -or
            -not [P3Acc.NativeJob]::ContainsProcess($jobState.Handle, $process.Handle)) { Throw-P3AccFailure 'P3ACC_CONTROLLER_APP_LAUNCH_FAILED' }
        Stop-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
        Unregister-ScheduledTask -TaskName $taskName -Confirm:$false -ErrorAction Stop
        if ($null -ne (Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue)) { Throw-P3AccFailure 'P3ACC_CONTROLLER_APP_LAUNCH_FAILED' }
        foreach ($path in @($preIdentityPath, ($preIdentityPath + '.tmp'), $handshakePath, ($handshakePath + '.tmp'), $launchPath)) {
            Remove-Item -LiteralPath $path -Force -ErrorAction SilentlyContinue
            if (Test-Path -LiteralPath $path) { Throw-P3AccFailure 'P3ACC_CONTROLLER_APP_LAUNCH_FAILED' }
        }
        $state = [pscustomobject]@{
            Process = $process; Identity = $identity; TaskName = $taskName; TaskRemoved = $true
            LauncherIdentity = $launcherIdentity; JobState = $jobState; LaunchPath = $launchPath
            ObservedDescendantIdentities = [Collections.ArrayList]::new()
            ObservedFfmpegIdentities = [Collections.ArrayList]::new()
        }
        $succeeded = $true
        return $state
    } catch {
        if ($_.Exception.Message -ceq 'P3ACC_CONTROLLER_CLEANUP_FAILED') { throw }
        Throw-P3AccFailure 'P3ACC_CONTROLLER_APP_LAUNCH_FAILED'
    } finally {
        if (-not $succeeded) {
            $cleanupOkay = $true
            if ($taskStarted -and $null -eq $launcherIdentity) {
                $identityDeadline = [DateTime]::UtcNow.AddSeconds(2)
                while ([DateTime]::UtcNow -lt $identityDeadline) {
                    if (Test-Path -LiteralPath $preIdentityPath -PathType Leaf) {
                        try {
                            $preDocument = Read-P3AccLauncherDocument -Configuration $Configuration -Path $preIdentityPath
                            $launcherIdentity = ConvertFrom-P3AccLauncherIdentityDocument -Document $preDocument -JobNonce $jobState.Nonce
                        } catch { $cleanupOkay = $false }
                        break
                    }
                    Start-Sleep -Milliseconds 50
                }
            }
            try {
                Stop-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
                Unregister-ScheduledTask -TaskName $taskName -Confirm:$false -ErrorAction SilentlyContinue
                if ($null -ne (Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue)) { $cleanupOkay = $false }
            } catch { $cleanupOkay = $false }
            if ($null -eq $launcherIdentity -and (Test-Path -LiteralPath $preIdentityPath -PathType Leaf)) {
                try {
                    $preDocument = Read-P3AccLauncherDocument -Configuration $Configuration -Path $preIdentityPath
                    $launcherIdentity = ConvertFrom-P3AccLauncherIdentityDocument -Document $preDocument -JobNonce $jobState.Nonce
                } catch { $cleanupOkay = $false }
            }
            if ($taskStarted -and $null -eq $launcherIdentity) { $cleanupOkay = $false }
            if ($null -ne $jobState) {
                if (-not (Stop-P3AccLauncherOutsideJob -JobState $jobState -LauncherIdentity $launcherIdentity)) { $cleanupOkay = $false }
                if (-not (Stop-P3AccJobState -JobState $jobState -TimeoutSeconds 30)) { $cleanupOkay = $false }
            }
            if ($null -ne $process) { try { $process.Dispose() } catch { } }
            foreach ($path in @($preIdentityPath, ($preIdentityPath + '.tmp'), $handshakePath, ($handshakePath + '.tmp'), $launchPath)) {
                Remove-Item -LiteralPath $path -Force -ErrorAction SilentlyContinue
                if (Test-Path -LiteralPath $path) { $cleanupOkay = $false }
            }
            if (($null -ne $launcherIdentity -and (Test-P3AccProcessIdentity $launcherIdentity)) -or
                ($null -ne $identity -and (Test-P3AccProcessIdentity $identity))) { $cleanupOkay = $false }
            if (-not $cleanupOkay) { $Configuration.AppLaunchCleanupUncertain = $true; Throw-P3AccFailure 'P3ACC_CONTROLLER_CLEANUP_FAILED' }
        }
    }
}

function Get-P3AccDescendantIdentities {
    param([Parameter(Mandatory)]$RootIdentity)
    if (-not (Test-P3AccProcessIdentity $RootIdentity)) { Throw-P3AccFailure 'P3ACC_CONTROLLER_TOPOLOGY_INVALID' }
    try { $processes = @(Get-CimInstance Win32_Process -ErrorAction Stop) }
    catch { Throw-P3AccFailure 'P3ACC_CONTROLLER_TOPOLOGY_INVALID' }
    if ($processes.Count -gt 4096) { Throw-P3AccFailure 'P3ACC_CONTROLLER_TOPOLOGY_INVALID' }
    $children = @{}
    foreach ($process in $processes) {
        $parent = [int]$process.ParentProcessId
        if (-not $children.ContainsKey($parent)) { $children[$parent] = [Collections.ArrayList]::new() }
        [void]$children[$parent].Add($process)
    }
    $queue = [Collections.Queue]::new()
    $queue.Enqueue([pscustomobject]@{ ProcessId = [int]$RootIdentity.ProcessId; StartedAtUtcTicks = [int64]$RootIdentity.StartedAtUtcTicks })
    $result = [Collections.ArrayList]::new()
    $seen = [Collections.Generic.HashSet[int]]::new()
    [void]$seen.Add([int]$RootIdentity.ProcessId)
    while ($queue.Count -gt 0) {
        if ($seen.Count -gt 512) { Throw-P3AccFailure 'P3ACC_CONTROLLER_TOPOLOGY_INVALID' }
        $parent = $queue.Dequeue()
        if (-not $children.ContainsKey([int]$parent.ProcessId)) { continue }
        foreach ($child in $children[[int]$parent.ProcessId]) {
            $pidValue = [int]$child.ProcessId
            if (-not $seen.Add($pidValue)) { Throw-P3AccFailure 'P3ACC_CONTROLLER_TOPOLOGY_INVALID' }
            try {
                $live = [Diagnostics.Process]::GetProcessById($pidValue)
                $startTicks = $live.StartTime.ToUniversalTime().Ticks
                $cimTicks = ([DateTime]$child.CreationDate).ToUniversalTime().Ticks
                if ($startTicks -lt [int64]$parent.StartedAtUtcTicks -or [Math]::Abs($startTicks - $cimTicks) -gt [TimeSpan]::FromSeconds(2).Ticks) {
                    $live.Dispose()
                    Throw-P3AccFailure 'P3ACC_CONTROLLER_TOPOLOGY_INVALID'
                }
                $identity = [pscustomobject]@{
                    ProcessId = $pidValue; StartedAtUtcTicks = $startTicks
                    ParentStartedAtUtcTicks = [int64]$parent.StartedAtUtcTicks
                    Name = [string]$child.Name; Process = $live
                }
                [void]$result.Add($identity)
                $queue.Enqueue([pscustomobject]@{ ProcessId = $pidValue; StartedAtUtcTicks = $startTicks })
            } catch {
                if ($_.Exception.Message -ceq 'P3ACC_CONTROLLER_TOPOLOGY_INVALID') { throw }
                if ((Test-P3AccExceptionChainType $_ ([ArgumentException])) -or
                    (Test-P3AccExceptionChainType $_ ([InvalidOperationException]))) { continue }
                Throw-P3AccFailure 'P3ACC_CONTROLLER_TOPOLOGY_INVALID'
            }
        }
    }
    return @($result)
}

function Add-P3AccObservedIdentity {
    param([Parameter(Mandatory)]$Collection, [Parameter(Mandatory)]$Identity)
    foreach ($known in @($Collection)) {
        if ([int]$known.ProcessId -eq [int]$Identity.ProcessId -and [int64]$known.StartedAtUtcTicks -eq [int64]$Identity.StartedAtUtcTicks) { return }
    }
    [void]$Collection.Add([pscustomobject]@{ ProcessId = [int]$Identity.ProcessId; StartedAtUtcTicks = [int64]$Identity.StartedAtUtcTicks })
}

function Register-P3AccCurrentDescendantIdentities {
    param([Parameter(Mandatory)]$AppState)
    if (-not (Test-P3AccProcessIdentity $AppState.Identity) -or
        $AppState.PSObject.Properties.Name -cnotcontains 'ObservedDescendantIdentities' -or
        $AppState.PSObject.Properties.Name -cnotcontains 'ObservedFfmpegIdentities') { Throw-P3AccFailure 'P3ACC_CONTROLLER_VISUAL_FAILED' }
    $descendants = @()
    try {
        $descendants = @(Get-P3AccDescendantIdentities $AppState.Identity)
        foreach ($identity in $descendants) {
            Add-P3AccObservedIdentity -Collection $AppState.ObservedDescendantIdentities -Identity $identity
            if ($identity.Name -ieq 'ffmpeg.exe') { Add-P3AccObservedIdentity -Collection $AppState.ObservedFfmpegIdentities -Identity $identity }
        }
        return $descendants.Count
    } finally {
        foreach ($identity in $descendants) { try { $identity.Process.Dispose() } catch { } }
    }
}

function Get-P3AccObservedTreeSurvivors {
    param([Parameter(Mandatory)]$AppState)
    if ($AppState.PSObject.Properties.Name -cnotcontains 'ObservedDescendantIdentities' -or
        $AppState.PSObject.Properties.Name -cnotcontains 'ObservedFfmpegIdentities') { Throw-P3AccFailure 'P3ACC_CONTROLLER_VISUAL_FAILED' }
    try { $processes = @(Get-CimInstance Win32_Process -ErrorAction Stop) }
    catch { Throw-P3AccFailure 'P3ACC_CONTROLLER_VISUAL_FAILED' }
    if ($processes.Count -gt 4096) { Throw-P3AccFailure 'P3ACC_CONTROLLER_VISUAL_FAILED' }
    $children = @{}
    foreach ($processRecord in $processes) {
        $parentId = [int]$processRecord.ParentProcessId
        if (-not $children.ContainsKey($parentId)) { $children[$parentId] = [Collections.ArrayList]::new() }
        [void]$children[$parentId].Add($processRecord)
    }
    $queue = [Collections.Queue]::new()
    $seen = @{}
    $seeds = @($AppState.Identity) + @($AppState.ObservedDescendantIdentities) + @($AppState.ObservedFfmpegIdentities)
    foreach ($seed in $seeds) {
        $seedId = [int]$seed.ProcessId
        $seedTicks = [int64]$seed.StartedAtUtcTicks
        if ($seedId -lt 1 -or $seedTicks -lt 1) { Throw-P3AccFailure 'P3ACC_CONTROLLER_VISUAL_FAILED' }
        $seedKey = [string]$seedId
        if ($seen.ContainsKey($seedKey)) {
            if ([int64]$seen[$seedKey] -ne $seedTicks) { Throw-P3AccFailure 'P3ACC_CONTROLLER_VISUAL_FAILED' }
            continue
        }
        $seen[$seedKey] = $seedTicks
        $queue.Enqueue([pscustomobject]@{ ProcessId = $seedId; StartedAtUtcTicks = $seedTicks })
    }
    $survivors = [Collections.ArrayList]::new()
    try {
        while ($queue.Count -gt 0) {
            if ($seen.Count -gt 512) { Throw-P3AccFailure 'P3ACC_CONTROLLER_VISUAL_FAILED' }
            $parent = $queue.Dequeue()
            if (-not $children.ContainsKey([int]$parent.ProcessId)) { continue }
            foreach ($child in $children[[int]$parent.ProcessId]) {
                $childId = [int]$child.ProcessId
                if ($childId -lt 1) { continue }
                $live = $null
                try {
                    $live = [Diagnostics.Process]::GetProcessById($childId)
                    $startTicks = $live.StartTime.ToUniversalTime().Ticks
                    $cimTicks = ([DateTime]$child.CreationDate).ToUniversalTime().Ticks
                } catch {
                    $lookupError = $_
                    if ($null -ne $live) { try { $live.Dispose() } catch { } }
                    if (-not (Test-P3AccExceptionChainType $lookupError ([ArgumentException])) -and
                        -not (Test-P3AccExceptionChainType $lookupError ([InvalidOperationException]))) {
                        Throw-P3AccFailure 'P3ACC_CONTROLLER_VISUAL_FAILED'
                    }
                    $confirmation = $null
                    $confirmedGone = $false
                    try {
                        $confirmation = [Diagnostics.Process]::GetProcessById($childId)
                        if ($confirmation.HasExited) {
                            $confirmedGone = $true
                        } else {
                            [void]$confirmation.StartTime.ToUniversalTime().Ticks
                            if ($confirmation.HasExited) { $confirmedGone = $true }
                        }
                    } catch {
                        if ((Test-P3AccExceptionChainType $_ ([ArgumentException])) -or
                            (Test-P3AccExceptionChainType $_ ([InvalidOperationException]))) {
                            $confirmedGone = $true
                        } else {
                            Throw-P3AccFailure 'P3ACC_CONTROLLER_VISUAL_FAILED'
                        }
                    } finally {
                        if ($null -ne $confirmation) { try { $confirmation.Dispose() } catch { } }
                    }
                    if ($confirmedGone) { continue }
                    Throw-P3AccFailure 'P3ACC_CONTROLLER_VISUAL_FAILED'
                }
                if ($startTicks -lt [int64]$parent.StartedAtUtcTicks -or
                    [Math]::Abs($startTicks - $cimTicks) -gt [TimeSpan]::FromSeconds(2).Ticks) {
                    $live.Dispose()
                    Throw-P3AccFailure 'P3ACC_CONTROLLER_VISUAL_FAILED'
                }
                $childKey = [string]$childId
                if ($seen.ContainsKey($childKey)) {
                    $knownTicks = [int64]$seen[$childKey]
                    $live.Dispose()
                    if ($knownTicks -ne $startTicks) { Throw-P3AccFailure 'P3ACC_CONTROLLER_VISUAL_FAILED' }
                    continue
                }
                $seen[$childKey] = $startTicks
                $identity = [pscustomobject]@{
                    ProcessId = $childId; StartedAtUtcTicks = $startTicks
                    ParentStartedAtUtcTicks = [int64]$parent.StartedAtUtcTicks
                    Name = [string]$child.Name; Process = $live
                }
                Add-P3AccObservedIdentity -Collection $AppState.ObservedDescendantIdentities -Identity $identity
                if ($identity.Name -ieq 'ffmpeg.exe') { Add-P3AccObservedIdentity -Collection $AppState.ObservedFfmpegIdentities -Identity $identity }
                [void]$survivors.Add($identity)
                $queue.Enqueue([pscustomobject]@{ ProcessId = $childId; StartedAtUtcTicks = $startTicks })
            }
        }
        return @($survivors)
    } catch {
        foreach ($identity in $survivors) { try { $identity.Process.Dispose() } catch { } }
        if ($_.Exception.Message -ceq 'P3ACC_CONTROLLER_VISUAL_FAILED') { throw }
        Throw-P3AccFailure 'P3ACC_CONTROLLER_VISUAL_FAILED'
    }
}

function Wait-P3AccNaturalAppTreeExit {
    param([Parameter(Mandatory)]$AppState, [int]$TimeoutSeconds = 10)
    if ($null -eq $AppState -or $AppState.PSObject.Properties.Name -cnotcontains 'JobState' -or
        $AppState.PSObject.Properties.Name -cnotcontains 'Process') { return $false }
    $jobState = $AppState.JobState
    if ($null -eq $jobState) { return $false }
    if ([bool]$jobState.Closed) { return [bool]$jobState.GracefulExitConfirmed }
    if (-not (Wait-P3AccJobEmpty -JobState $jobState -TimeoutSeconds $TimeoutSeconds -ConsecutiveZeroSamples 2 -PollMilliseconds 100)) { return $false }
    try {
        $jobState.GracefulExitConfirmed = $true
        if (-not (Close-P3AccJobState $jobState)) {
            $jobState.GracefulExitConfirmed = $false
            return $false
        }
        try { $AppState.Process.Dispose() } catch { }
        return $true
    } catch {
        return $false
    }
}

function Test-P3AccLoopbackAddress {
    param([string]$Value)
    try {
        $address = [Net.IPAddress]::Parse($Value)
        if ($address.IsIPv4MappedToIPv6) { $address = $address.MapToIPv4() }
        return [Net.IPAddress]::IsLoopback($address)
    }
    catch { return $false }
}

function Test-P3AccAddressEquals {
    param([string]$Left, [string]$Right)
    try {
        $leftAddress = [Net.IPAddress]::Parse($Left)
        $rightAddress = [Net.IPAddress]::Parse($Right)
        if ($leftAddress.IsIPv4MappedToIPv6) { $leftAddress = $leftAddress.MapToIPv4() }
        if ($rightAddress.IsIPv4MappedToIPv6) { $rightAddress = $rightAddress.MapToIPv4() }
        return $leftAddress.Equals($rightAddress)
    }
    catch { return $false }
}

function Test-P3AccExceptionChainType {
    param($ErrorRecord, [Parameter(Mandatory)][type]$ExpectedType)
    if ($null -eq $ErrorRecord) { return $false }
    $exception = $ErrorRecord.Exception
    while ($null -ne $exception) {
        if ($ExpectedType.IsAssignableFrom($exception.GetType())) { return $true }
        $exception = $exception.InnerException
    }
    return $false
}

function Get-P3AccProcessIdentityState {
    param([Parameter(Mandatory)]$Identity)
    $current = $null
    try {
        $current = [Diagnostics.Process]::GetProcessById([int]$Identity.ProcessId)
        if ($current.HasExited) { return 'Exited' }
        $ticks = $current.StartTime.ToUniversalTime().Ticks
        if ($current.HasExited) { return 'Exited' }
        if ($ticks -ne [int64]$Identity.StartedAtUtcTicks) { return 'Reused' }
        return 'Alive'
    } catch {
        if ((Test-P3AccExceptionChainType $_ ([ArgumentException])) -or
            (Test-P3AccExceptionChainType $_ ([InvalidOperationException]))) { return 'Exited' }
        Throw-P3AccFailure 'P3ACC_CONTROLLER_TOPOLOGY_INVALID'
    } finally {
        if ($null -ne $current) { try { $current.Dispose() } catch { } }
    }
}

function New-P3AccRetryableTopologySample {
    return [pscustomobject]@{
        Complete = $false; RetryableUnstable = $true
        AppOnlyRelay = $false; FfmpegOnlyRelay = $false; RelayOnlyUpstream = $false; NoUdpBypass = $true
        AppIdentityKey = ''; RelayIdentityKey = ''; FfmpegIdentityKey = ''
    }
}

function Test-P3AccRelayPeerRow {
    param([Parameter(Mandatory)]$ClientConnection, [Parameter(Mandatory)][AllowEmptyCollection()][object[]]$RelayConnections, [Parameter(Mandatory)][int]$RelayPort)
    foreach ($relayConnection in $RelayConnections) {
        if ([string]$relayConnection.State -cne 'Established' -or
            [int]$relayConnection.LocalPort -ne $RelayPort -or
            [int]$relayConnection.RemotePort -ne [int]$ClientConnection.LocalPort -or
            -not (Test-P3AccLoopbackAddress $relayConnection.LocalAddress) -or
            -not (Test-P3AccLoopbackAddress $relayConnection.RemoteAddress) -or
            -not (Test-P3AccAddressEquals $relayConnection.LocalAddress $ClientConnection.RemoteAddress) -or
            -not (Test-P3AccAddressEquals $relayConnection.RemoteAddress $ClientConnection.LocalAddress)) { continue }
        return $true
    }
    return $false
}

function Test-P3AccUdpBypass {
    param([Parameter(Mandatory)][AllowEmptyCollection()][object[]]$UdpEndpoints, [Parameter(Mandatory)][AllowEmptyCollection()][int[]]$ClientIds)
    foreach ($endpoint in $UdpEndpoints) {
        if ($ClientIds -contains [int]$endpoint.OwningProcess) { return $true }
    }
    return $false
}

function Get-P3AccTopologyClientScope {
    param(
        [Parameter(Mandatory)]$RootIdentity,
        [Parameter(Mandatory)][AllowEmptyCollection()][object[]]$Descendants,
        [Parameter(Mandatory)]$RelayIdentity
    )
    $ffmpegIdentities = @($Descendants | Where-Object { $_.Name -ieq 'ffmpeg.exe' })
    $appClientIds = @([int]$RootIdentity.ProcessId) + @($Descendants | Where-Object { $_.Name -ine 'ffmpeg.exe' } | ForEach-Object { [int]$_.ProcessId })
    $ffmpegClientIds = @($ffmpegIdentities | ForEach-Object { [int]$_.ProcessId })
    $clientIds = @($appClientIds) + @($ffmpegClientIds)
    return [pscustomobject]@{
        FfmpegIdentities = @($ffmpegIdentities)
        AppClientIds = @($appClientIds)
        FfmpegClientIds = @($ffmpegClientIds)
        ClientIds = @($clientIds)
        UdpForbiddenIds = @($clientIds) + @([int]$RelayIdentity.ProcessId)
    }
}

function Get-P3AccTopologySample {
    param([Parameter(Mandatory)]$Configuration, [Parameter(Mandatory)]$AppState, [Parameter(Mandatory)]$RelayState)
    if (-not (Test-P3AccProcessIdentity $AppState.Identity) -or -not (Test-P3AccProcessIdentity $RelayState.Identity)) { Throw-P3AccFailure 'P3ACC_CONTROLLER_TOPOLOGY_INVALID' }
    $descendants = @()
    try {
        $descendants = @(Get-P3AccDescendantIdentities $AppState.Identity)
        $clientScope = Get-P3AccTopologyClientScope -RootIdentity $AppState.Identity -Descendants $descendants -RelayIdentity $RelayState.Identity
        $ffmpeg = @($clientScope.FfmpegIdentities)
        $appClientIds = @($clientScope.AppClientIds)
        $ffmpegClientIds = @($clientScope.FfmpegClientIds)
        foreach ($identity in $descendants) {
            Add-P3AccObservedIdentity -Collection $AppState.ObservedDescendantIdentities -Identity $identity
        }
        foreach ($identity in $ffmpeg) { Add-P3AccObservedIdentity -Collection $AppState.ObservedFfmpegIdentities -Identity $identity }
        try {
            $tcp = @(Get-NetTCPConnection -ErrorAction Stop | Where-Object { [int]$_.RemotePort -gt 0 })
            $udp = @(Get-NetUDPEndpoint -ErrorAction Stop)
        } catch { Throw-P3AccFailure 'P3ACC_CONTROLLER_TOPOLOGY_INVALID' }
        $appConnections = @($tcp | Where-Object { $appClientIds -contains [int]$_.OwningProcess })
        $ffmpegConnections = @($tcp | Where-Object { $ffmpegClientIds -contains [int]$_.OwningProcess })
        $relayConnections = @($tcp | Where-Object { [int]$_.OwningProcess -eq [int]$RelayState.Identity.ProcessId })
        foreach ($connection in $appConnections) { if (-not (Test-P3AccLoopbackAddress $connection.RemoteAddress) -or [int]$connection.RemotePort -ne $RelayState.Port) { Throw-P3AccFailure 'P3ACC_CONTROLLER_TOPOLOGY_INVALID' } }
        foreach ($connection in $ffmpegConnections) { if (-not (Test-P3AccLoopbackAddress $connection.RemoteAddress) -or [int]$connection.RemotePort -ne $RelayState.Port) { Throw-P3AccFailure 'P3ACC_CONTROLLER_TOPOLOGY_INVALID' } }
        $relayClientConnections = [Collections.ArrayList]::new()
        $upstreamEstablished = 0
        foreach ($connection in $relayConnections) {
            if ([int]$connection.LocalPort -eq $RelayState.Port -and (Test-P3AccLoopbackAddress $connection.LocalAddress) -and (Test-P3AccLoopbackAddress $connection.RemoteAddress)) {
                [void]$relayClientConnections.Add($connection)
                continue
            }
            if ([int]$connection.RemotePort -eq $Configuration.Upstream.Port -and (Test-P3AccAddressEquals $connection.RemoteAddress $Configuration.Upstream.Host)) {
                if ([string]$connection.State -ceq 'Established') { $upstreamEstablished++ }
                continue
            }
            Throw-P3AccFailure 'P3ACC_CONTROLLER_TOPOLOGY_INVALID'
        }
        $udpBypass = Test-P3AccUdpBypass -UdpEndpoints $udp -ClientIds @($clientScope.UdpForbiddenIds)
        if ($udpBypass) { Throw-P3AccFailure 'P3ACC_CONTROLLER_TOPOLOGY_INVALID' }
        if (-not (Test-P3AccProcessIdentity $AppState.Identity) -or -not (Test-P3AccProcessIdentity $RelayState.Identity)) { Throw-P3AccFailure 'P3ACC_CONTROLLER_TOPOLOGY_INVALID' }
        foreach ($identity in $descendants) {
            $identityState = Get-P3AccProcessIdentityState $identity
            if ($identityState -ceq 'Reused') { Throw-P3AccFailure 'P3ACC_CONTROLLER_TOPOLOGY_INVALID' }
            if ($identityState -ceq 'Exited') { return New-P3AccRetryableTopologySample }
        }
        if ($ffmpeg.Count -ne 1) { return New-P3AccRetryableTopologySample }

        $appEstablished = @($appConnections | Where-Object { [string]$_.State -ceq 'Established' })
        $ffmpegEstablished = @($ffmpegConnections | Where-Object { [string]$_.State -ceq 'Established' })
        $rootEstablished = @($appEstablished | Where-Object { [int]$_.OwningProcess -eq [int]$AppState.Identity.ProcessId })
        $appPeersMatched = $rootEstablished.Count -gt 0
        foreach ($connection in $appEstablished) {
            if (-not (Test-P3AccRelayPeerRow -ClientConnection $connection -RelayConnections @($relayClientConnections) -RelayPort $RelayState.Port)) { $appPeersMatched = $false; break }
        }
        $ffmpegPeersMatched = $ffmpegEstablished.Count -gt 0
        foreach ($connection in $ffmpegEstablished) {
            if (-not (Test-P3AccRelayPeerRow -ClientConnection $connection -RelayConnections @($relayClientConnections) -RelayPort $RelayState.Port)) { $ffmpegPeersMatched = $false; break }
        }
        $appOnlyRelay = $appPeersMatched
        $ffmpegOnlyRelay = $ffmpegPeersMatched
        $relayOnlyUpstream = $appPeersMatched -and $ffmpegPeersMatched -and $upstreamEstablished -gt 0
        $complete = $appOnlyRelay -and $ffmpegOnlyRelay -and $relayOnlyUpstream
        if (-not $complete) { return New-P3AccRetryableTopologySample }
        return [pscustomobject]@{
            Complete = $true; RetryableUnstable = $false
            AppOnlyRelay = $true; FfmpegOnlyRelay = $true; RelayOnlyUpstream = $true; NoUdpBypass = $true
            AppIdentityKey = ('{0}:{1}' -f [int]$AppState.Identity.ProcessId,[int64]$AppState.Identity.StartedAtUtcTicks)
            RelayIdentityKey = ('{0}:{1}' -f [int]$RelayState.Identity.ProcessId,[int64]$RelayState.Identity.StartedAtUtcTicks)
            FfmpegIdentityKey = ('{0}:{1}' -f [int]$ffmpeg[0].ProcessId,[int64]$ffmpeg[0].StartedAtUtcTicks)
        }
    } finally {
        foreach ($item in $descendants) { try { $item.Process.Dispose() } catch { } }
    }
}

function New-P3AccTopologyTracker {
    return [pscustomobject]@{
        SampleCount = 0; BeforeFaultSampleCount = 0; AfterRecoverySampleCount = 0
        AppOnlyRelay = $false; FfmpegOnlyRelay = $false; RelayOnlyUpstream = $false; NoUdpBypass = $false
        BeforeFaultEpochKey = ''; BeforeFaultIdentityKey = ''; BeforeFaultLastCapturedAt = [int64]0
        AfterRecoveryEpochKey = ''; AfterRecoveryIdentityKey = ''; AfterRecoveryLastCapturedAt = [int64]0
        BeforeFaultAttempted = 0; BeforeFaultInconclusive = 0
        AfterRecoveryAttempted = 0; AfterRecoveryInconclusive = 0
    }
}

function Update-P3AccTopologyTrackerSummary {
    param([Parameter(Mandatory)]$Tracker)
    $Tracker.SampleCount = [int]$Tracker.BeforeFaultSampleCount + [int]$Tracker.AfterRecoverySampleCount
    $proven = [int]$Tracker.BeforeFaultSampleCount -ge 3 -and [int]$Tracker.AfterRecoverySampleCount -ge 3
    $Tracker.AppOnlyRelay = $proven
    $Tracker.FfmpegOnlyRelay = $proven
    $Tracker.RelayOnlyUpstream = $proven
    $Tracker.NoUdpBypass = $proven
}

function Reset-P3AccTopologyPhase {
    param([Parameter(Mandatory)]$Tracker, [switch]$AfterRecovery)
    $prefix = if ($AfterRecovery) { 'AfterRecovery' } else { 'BeforeFault' }
    $Tracker.($prefix + 'SampleCount') = 0
    $Tracker.($prefix + 'EpochKey') = ''
    $Tracker.($prefix + 'IdentityKey') = ''
    $Tracker.($prefix + 'LastCapturedAt') = [int64]0
    Update-P3AccTopologyTrackerSummary $Tracker
}

function Test-P3AccTopologySnapshotEligible {
    param($Snapshot, [switch]$AfterRecovery)
    if ($null -eq $Snapshot) { return $false }
    try {
        [void](Assert-P3AccSnapshotContract $Snapshot)
        $base = $Snapshot.stage -ceq 'RECOVERED' -and
            $Snapshot.runtime.state -ceq 'RECORDING' -and $Snapshot.runtime.recordingStatus -ceq 'recording' -and
            $Snapshot.runtime.hasSession -and $Snapshot.runtime.sessionFenceStable -and
            $Snapshot.runtime.currentAttemptCommitted -and $Snapshot.runtime.recorderTargetMatched
        if (-not $base) { return $false }
        if ($AfterRecovery) {
            return [int]$Snapshot.runtime.attemptCount -ge 3 -and [int]$Snapshot.progress.restartCount -ge 2 -and
                [bool](Test-P3AccNetworkRecovered $Snapshot)
        }
        return [int]$Snapshot.runtime.attemptCount -ge 2 -and [int]$Snapshot.progress.restartCount -ge 1 -and
            [bool](Test-P3AccPreFaultReady $Snapshot)
    } catch { return $false }
}

function Test-P3AccTopologyFenceStable {
    param($BeforeSnapshot, $AfterSnapshot, [switch]$AfterRecovery)
    if (-not (Test-P3AccTopologySnapshotEligible $BeforeSnapshot -AfterRecovery:$AfterRecovery) -or
        -not (Test-P3AccTopologySnapshotEligible $AfterSnapshot -AfterRecovery:$AfterRecovery)) { return $false }
    return [int64]$AfterSnapshot.capturedAt -ge [int64]$BeforeSnapshot.capturedAt -and
        [int64]$BeforeSnapshot.runtime.revision -eq [int64]$AfterSnapshot.runtime.revision -and
        [int]$BeforeSnapshot.runtime.attemptCount -eq [int]$AfterSnapshot.runtime.attemptCount -and
        [int]$BeforeSnapshot.progress.restartCount -eq [int]$AfterSnapshot.progress.restartCount
}

function Assert-P3AccTopologySample {
    param([Parameter(Mandatory)]$Sample)
    $required = @('Complete','RetryableUnstable','AppOnlyRelay','FfmpegOnlyRelay','RelayOnlyUpstream','NoUdpBypass','AppIdentityKey','RelayIdentityKey','FfmpegIdentityKey')
    foreach ($name in $required) { if ($Sample.PSObject.Properties.Name -cnotcontains $name) { Throw-P3AccFailure 'P3ACC_CONTROLLER_TOPOLOGY_INVALID' } }
    foreach ($name in @('Complete','RetryableUnstable','AppOnlyRelay','FfmpegOnlyRelay','RelayOnlyUpstream','NoUdpBypass')) {
        if ($Sample.$name -isnot [bool]) { Throw-P3AccFailure 'P3ACC_CONTROLLER_TOPOLOGY_INVALID' }
    }
    foreach ($name in @('AppIdentityKey','RelayIdentityKey','FfmpegIdentityKey')) {
        if ($Sample.$name -isnot [string]) { Throw-P3AccFailure 'P3ACC_CONTROLLER_TOPOLOGY_INVALID' }
    }
    if (-not $Sample.NoUdpBypass -or ($Sample.Complete -and ($Sample.RetryableUnstable -or -not $Sample.AppOnlyRelay -or
        -not $Sample.FfmpegOnlyRelay -or -not $Sample.RelayOnlyUpstream -or
        [string]::IsNullOrEmpty($Sample.AppIdentityKey) -or [string]::IsNullOrEmpty($Sample.RelayIdentityKey) -or [string]::IsNullOrEmpty($Sample.FfmpegIdentityKey))) -or
        (-not $Sample.Complete -and -not $Sample.RetryableUnstable)) { Throw-P3AccFailure 'P3ACC_CONTROLLER_TOPOLOGY_INVALID' }
}

function Add-P3AccTopologySample {
    param($Tracker, $Sample, $Snapshot, $ConfirmedSnapshot, [switch]$AfterRecovery)
    Assert-P3AccTopologySample $Sample
    $prefix = if ($AfterRecovery) { 'AfterRecovery' } else { 'BeforeFault' }
    $Tracker.($prefix + 'Attempted')++
    if ($Sample.RetryableUnstable -or -not (Test-P3AccTopologyFenceStable $Snapshot $ConfirmedSnapshot -AfterRecovery:$AfterRecovery)) {
        $Tracker.($prefix + 'Inconclusive')++
        Reset-P3AccTopologyPhase -Tracker $Tracker -AfterRecovery:$AfterRecovery
        return $false
    }
    $capturedAt = [int64]$ConfirmedSnapshot.capturedAt
    $lastCapturedAt = [int64]$Tracker.($prefix + 'LastCapturedAt')
    if ($capturedAt -le $lastCapturedAt) {
        if ($capturedAt -lt $lastCapturedAt) { Reset-P3AccTopologyPhase -Tracker $Tracker -AfterRecovery:$AfterRecovery }
        return $false
    }
    $epochKey = '{0}:{1}:{2}' -f [int64]$ConfirmedSnapshot.runtime.revision,[int]$ConfirmedSnapshot.runtime.attemptCount,[int]$ConfirmedSnapshot.progress.restartCount
    $identityKey = '{0}|{1}|{2}' -f $Sample.AppIdentityKey,$Sample.RelayIdentityKey,$Sample.FfmpegIdentityKey
    $currentEpochKey = [string]$Tracker.($prefix + 'EpochKey')
    $currentIdentityKey = [string]$Tracker.($prefix + 'IdentityKey')
    if (($currentEpochKey.Length -gt 0 -and $currentEpochKey -cne $epochKey) -or
        ($currentIdentityKey.Length -gt 0 -and $currentIdentityKey -cne $identityKey) -or
        ($lastCapturedAt -gt 0 -and $capturedAt - $lastCapturedAt -gt 30000)) {
        Reset-P3AccTopologyPhase -Tracker $Tracker -AfterRecovery:$AfterRecovery
    }
    $Tracker.($prefix + 'EpochKey') = $epochKey
    $Tracker.($prefix + 'IdentityKey') = $identityKey
    $Tracker.($prefix + 'LastCapturedAt') = $capturedAt
    $Tracker.($prefix + 'SampleCount')++
    Update-P3AccTopologyTrackerSummary $Tracker
    return $true
}

function Test-P3AccTopologyPhaseReady {
    param([Parameter(Mandatory)]$Tracker, [switch]$AfterRecovery)
    if ($AfterRecovery) { return [int]$Tracker.AfterRecoverySampleCount -ge 3 }
    return [int]$Tracker.BeforeFaultSampleCount -ge 3
}

function Copy-P3AccTopologyTrackerToReport {
    param([Parameter(Mandatory)]$Tracker, [Parameter(Mandatory)]$Report)
    Update-P3AccTopologyTrackerSummary $Tracker
    $Report.topology.sampleCount = [int]$Tracker.SampleCount
    $Report.topology.beforeFaultSampleCount = [int]$Tracker.BeforeFaultSampleCount
    $Report.topology.afterRecoverySampleCount = [int]$Tracker.AfterRecoverySampleCount
    $Report.topology.appOnlyRelay = [bool]$Tracker.AppOnlyRelay
    $Report.topology.ffmpegOnlyRelay = [bool]$Tracker.FfmpegOnlyRelay
    $Report.topology.relayOnlyUpstream = [bool]$Tracker.RelayOnlyUpstream
    $Report.topology.noUdpBypass = [bool]$Tracker.NoUdpBypass
}

function Test-P3AccPreFaultReady {
    param($Snapshot)
    return $Snapshot.ui.ready -and $Snapshot.ui.recordingSeen -and $Snapshot.ui.progressAdvanced -and $Snapshot.ui.timelineSeen -and
        $Snapshot.ui.reconnectingSeen -and $Snapshot.ui.recoveredSeen -and
        [int64]$Snapshot.resources.sampleCount -ge 30 -and [int64]$Snapshot.resources.windowDurationMs -ge 600000 -and
        $Snapshot.resources.sampleComplete -and $Snapshot.resources.stableWindowProven -and $Snapshot.resources.cpuWithinTarget -and
        (Test-P3AccResourceObservationInvariants $Snapshot.resources -RequireObserved -SnapshotDerived) -and
        $Snapshot.runtime.crashInjected -and $Snapshot.runtime.recoveryProven -and $Snapshot.runtime.attemptAdvanced -and
        $Snapshot.gaps.crashRecoveryMatched -and $Snapshot.runtime.networkFaultArmed -and
        $Snapshot.runtime.state -ceq 'RECORDING' -and $Snapshot.runtime.recordingStatus -ceq 'recording' -and
        $Snapshot.runtime.sessionFenceStable -and $Snapshot.runtime.currentAttemptCommitted -and $Snapshot.runtime.recorderTargetMatched
}

function Test-P3AccFaultObserved {
    param($Snapshot)
    return $Snapshot.runtime.networkFaultArmed -and $Snapshot.runtime.state -ceq 'RECONNECTING' -and
        $Snapshot.ui.networkReconnectingSeen -and [int]$Snapshot.gaps.openMessageDisconnect -gt 0 -and
        [int]$Snapshot.gaps.openRecordingRestart -gt 0
}

function Test-P3AccNetworkRecovered {
    param($Snapshot)
    return $Snapshot.runtime.networkRecoveryProven -and $Snapshot.ui.networkRecoveredSeen -and
        $Snapshot.gaps.networkMessageMatched -and $Snapshot.gaps.networkRecorderMatched -and
        $Snapshot.runtime.state -ceq 'RECORDING' -and $Snapshot.runtime.recordingStatus -ceq 'recording' -and
        $Snapshot.runtime.hasSession -and $Snapshot.runtime.sessionFenceStable -and
        $Snapshot.runtime.currentAttemptCommitted -and $Snapshot.runtime.recorderTargetMatched
}

function Test-P3AccCleanOfflineTerminalOutcome {
    param($SessionStatus, $RecordingStatus)
    return $SessionStatus -is [string] -and
        $RecordingStatus -is [string] -and
        $SessionStatus -ceq 'completed' -and
        $RecordingStatus -ceq 'incomplete'
}

function Test-P3AccFinalContract {
    param($Snapshot)
    $allUi = $Snapshot.ui.ready -and $Snapshot.ui.recordingSeen -and $Snapshot.ui.progressAdvanced -and $Snapshot.ui.timelineSeen -and
        $Snapshot.ui.reconnectingSeen -and $Snapshot.ui.recoveredSeen -and $Snapshot.ui.networkReconnectingSeen -and
        $Snapshot.ui.networkRecoveredSeen -and $Snapshot.ui.offlineSeen -and $Snapshot.ui.finalizedSeen
    $mediaTerminalValid = ($Snapshot.mediaManifest.state -ceq 'completed' -and [int]$Snapshot.mediaManifest.incompleteSegmentCount -eq 0) -or
        ($Snapshot.mediaManifest.state -ceq 'incomplete' -and [int]$Snapshot.mediaManifest.incompleteSegmentCount -gt 0)
    $sessionTerminalValid = Test-P3AccCleanOfflineTerminalOutcome -SessionStatus $Snapshot.sessionManifest.status -RecordingStatus $Snapshot.sessionManifest.recordingStatus
    if ($Snapshot.stage -cne 'FINALIZED' -or -not $allUi -or [int]$Snapshot.ui.observationCount -ne 10 -or
        [int]$Snapshot.ui.latencySampleCount -lt 1 -or [int]$Snapshot.ui.latencyPendingCount -ne 0 -or
        [int]$Snapshot.ui.latencyP95Ms -ge 1000 -or -not $Snapshot.ui.latencyWithinTarget -or
        $Snapshot.runtime.state -cne 'WAITING' -or $Snapshot.runtime.errorCode -cne 'ROOM_OFFLINE' -or
        $Snapshot.runtime.recordingStatus -cne '' -or -not $Snapshot.runtime.hasSession -or
        -not $Snapshot.runtime.crashInjected -or -not $Snapshot.runtime.recoveryProven -or
        -not $Snapshot.runtime.networkFaultArmed -or -not $Snapshot.runtime.networkRecoveryProven -or -not $Snapshot.runtime.finalizationProven -or
        [int]$Snapshot.database.sessionCount -ne 1 -or [int]$Snapshot.database.activeSessionCount -ne 0 -or
        [int]$Snapshot.database.publishedEventCount -lt 1 -or -not $Snapshot.database.publishedEventsPersisted -or
        [int]$Snapshot.gaps.open -ne 0 -or [int]$Snapshot.gaps.openRecordingRestart -ne 0 -or [int]$Snapshot.gaps.openMessageDisconnect -ne 0 -or
        -not $Snapshot.gaps.crashRecoveryMatched -or -not $Snapshot.gaps.networkMessageMatched -or -not $Snapshot.gaps.networkRecorderMatched -or
        -not $Snapshot.checkpoint.exists -or $Snapshot.checkpoint.state -cne 'closed' -or [int]$Snapshot.checkpoint.openGiftFoldCount -ne 0 -or
        -not $Snapshot.checkpoint.coversSourceEvents -or -not $Snapshot.checkpoint.giftFoldsClosed -or
        -not $Snapshot.sessionManifest.exists -or -not $Snapshot.sessionManifest.matchesDatabase -or
        -not $Snapshot.sessionManifest.canonicalHashMatches -or -not $Snapshot.sessionManifest.manifestClean -or
        -not $sessionTerminalValid -or -not $Snapshot.sessionManifest.ended -or
        -not $Snapshot.mediaManifest.exists -or -not $Snapshot.mediaManifest.matchesDatabase -or
        -not $Snapshot.mediaManifest.canonicalHashMatches -or -not $Snapshot.mediaManifest.manifestClean -or -not $mediaTerminalValid -or
        -not $Snapshot.mediaManifest.allFilesMatch -or -not $Snapshot.mediaManifest.sequenceContinuous -or
        -not $Snapshot.mediaManifest.attemptReferencesValid -or -not $Snapshot.mediaManifest.faultPhaseSegmentsProven -or
        [int]$Snapshot.mediaManifest.fileFailureCount -ne 0 -or [int]$Snapshot.mediaManifest.fileCheckCount -lt 1 -or
        [int]$Snapshot.mediaManifest.segmentCount -lt 1 -or
        [int]$Snapshot.mediaManifest.fileCheckCount + [int]$Snapshot.mediaManifest.incompleteEntryCount -ne [int]$Snapshot.mediaManifest.segmentCount + [int]$Snapshot.mediaManifest.artifactCount -or
        -not $Snapshot.resources.sampleComplete -or -not $Snapshot.resources.stableWindowProven -or
        -not $Snapshot.resources.cpuWithinTarget -or [int]$Snapshot.resources.sampleCount -lt 30 -or
        [int64]$Snapshot.resources.windowDurationMs -lt 600000 -or [double]$Snapshot.resources.averageCpuPercent -ge 10 -or
        -not (Test-P3AccResourceObservationInvariants $Snapshot.resources -RequireObserved -SnapshotDerived)) {
        return $false
    }
    return $true
}

function Wait-P3AccSnapshot {
    param([Parameter(Mandatory)]$Configuration, [Parameter(Mandatory)][int]$TimeoutSeconds, [Parameter(Mandatory)][scriptblock]$Predicate)
    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    while ([DateTime]::UtcNow -lt $deadline) {
        $snapshot = Read-P3AccSnapshot $Configuration
        if ($null -ne $snapshot) {
            if ($snapshot.stage -ceq 'ERROR') { Throw-P3AccFailure 'P3ACC_CONTROLLER_FINAL_INVALID' }
            if (& $Predicate $snapshot) { return $snapshot }
        }
        Start-Sleep -Milliseconds $Configuration.PollIntervalMilliseconds
    }
    Throw-P3AccFailure 'P3ACC_CONTROLLER_TIMEOUT'
}

function Save-P3AccSafeStatusCrop {
    param([Parameter(Mandatory)]$Configuration, [Parameter(Mandatory)]$AppState)
    if (-not (Test-P3AccProcessIdentity $AppState.Identity)) { Throw-P3AccFailure 'P3ACC_CONTROLLER_VISUAL_FAILED' }
    Assert-P3AccRootStillOwned $Configuration
    $path = [IO.Path]::Combine($Configuration.Root, 'p3-acc-safe-status.png')
    try { $capture = [P3Acc.NativeWindow]::CaptureSafeCrop([uint32]$AppState.Identity.ProcessId, $path) }
    catch { Throw-P3AccFailure 'P3ACC_CONTROLLER_VISUAL_FAILED' }
    Assert-P3AccRootStillOwned $Configuration
    if (-not $capture.Captured -or -not $capture.NonUniform -or $capture.Width -lt 220 -or $capture.Height -lt 120 -or -not (Test-Path -LiteralPath $path -PathType Leaf) -or (Get-Item -LiteralPath $path).Length -lt 256) {
        Throw-P3AccFailure 'P3ACC_CONTROLLER_VISUAL_FAILED'
    }
    return [pscustomobject]@{ Path = $path; Width = $capture.Width; Height = $capture.Height; NonUniform = $capture.NonUniform }
}

function Wait-P3AccEvidenceAcknowledgement {
    param([Parameter(Mandatory)]$Configuration, [Parameter(Mandatory)]$Capture, [Parameter(Mandatory)]$AppState, [int]$TimeoutSeconds)
    try {
        [void](Assert-P3AccControlRootStillOwned $Configuration)
        Assert-P3AccRootStillOwned $Configuration
        [void](Assert-P3AccNoReparsePath -Path $Capture.Path -Directory $false)
        $hash = (Get-FileHash -LiteralPath $Capture.Path -Algorithm SHA256 -ErrorAction Stop).Hash.ToLowerInvariant()
    } catch { Throw-P3AccFailure 'P3ACC_CONTROLLER_VISUAL_FAILED' }
    if ($hash -cnotmatch '^[0-9a-f]{64}$') { Throw-P3AccFailure 'P3ACC_CONTROLLER_VISUAL_FAILED' }
    $readyPath = [IO.Path]::Combine($Configuration.Root, 'evidence.ready')
    $ackPath = [IO.Path]::Combine($Configuration.Root, 'evidence.ack')
    if ((Test-Path -LiteralPath $readyPath) -or (Test-Path -LiteralPath $ackPath)) { Throw-P3AccFailure 'P3ACC_CONTROLLER_VISUAL_FAILED' }
    $payload = [ordered]@{ schema = 'P3ACC-EVIDENCE/v1'; sha256 = $hash; width = [int]$Capture.Width; height = [int]$Capture.Height } | ConvertTo-Json -Compress
    $temporary = [IO.Path]::Combine($Configuration.ControlRoot, 'evidence.ready.tmp')
    try {
        [void](Assert-P3AccControlRootStillOwned $Configuration)
        Assert-P3AccRootStillOwned $Configuration
        [IO.File]::WriteAllText($temporary, $payload, [Text.UTF8Encoding]::new($false))
        [void](Assert-P3AccControlRootStillOwned $Configuration)
        [void](Assert-P3AccNoReparsePath -Path $temporary -Directory $false)
        Assert-P3AccRootStillOwned $Configuration
        Move-Item -LiteralPath $temporary -Destination $readyPath -ErrorAction Stop
        [void](Assert-P3AccControlRootStillOwned $Configuration)
        Assert-P3AccRootStillOwned $Configuration
        [void](Assert-P3AccNoReparsePath -Path $readyPath -Directory $false)
    } catch {
        if ($_.Exception.Message -ceq 'P3ACC_CONTROLLER_VISUAL_FAILED') { throw }
        Throw-P3AccFailure 'P3ACC_CONTROLLER_VISUAL_FAILED'
    }
    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    while ([DateTime]::UtcNow -lt $deadline) {
        if (-not (Test-P3AccProcessIdentity $AppState.Identity)) { Throw-P3AccFailure 'P3ACC_CONTROLLER_VISUAL_FAILED' }
        try {
            [void](Assert-P3AccControlRootStillOwned $Configuration)
            Assert-P3AccRootStillOwned $Configuration
        } catch { Throw-P3AccFailure 'P3ACC_CONTROLLER_VISUAL_FAILED' }
        if (Test-Path -LiteralPath $ackPath -PathType Leaf) {
            try {
                [void](Assert-P3AccControlRootStillOwned $Configuration)
                Assert-P3AccRootStillOwned $Configuration
                $ackInfo = Assert-P3AccNoReparsePath -Path $ackPath -Directory $false
                if ((Get-Item -LiteralPath $ackInfo.Path).Length -gt 1024) { Throw-P3AccFailure 'P3ACC_CONTROLLER_VISUAL_FAILED' }
                $ack = Get-Content -LiteralPath $ackInfo.Path -Raw -ErrorAction Stop | ConvertFrom-Json -ErrorAction Stop
                Assert-P3AccRootStillOwned $Configuration
                Assert-P3AccProperties $ack @('schema','sha256')
                if ($ack.schema -cne 'P3ACC-EVIDENCE-ACK/v1' -or $ack.sha256 -cne $hash) { Throw-P3AccFailure 'P3ACC_CONTROLLER_VISUAL_FAILED' }
                return [pscustomobject]@{ SHA256 = $hash; Acknowledged = $true }
            } catch {
                if ($_.Exception.Message -ceq 'P3ACC_CONTROLLER_VISUAL_FAILED') { throw }
                Throw-P3AccFailure 'P3ACC_CONTROLLER_VISUAL_FAILED'
            }
        }
        Start-Sleep -Milliseconds 250
    }
    Throw-P3AccFailure 'P3ACC_CONTROLLER_TIMEOUT'
}

function Write-P3AccInteractiveVisualHelper {
    param($Configuration, $AppState, [int]$EvidenceTimeoutSeconds, [int]$CloseTimeoutSeconds, [string]$HelperPath, [string]$ResultPath)
    $module = ConvertTo-P3AccSingleQuotedLiteral $PSCommandPath
    [void](Assert-P3AccControlRootStillOwned $Configuration)
    $root = ConvertTo-P3AccSingleQuotedLiteral $Configuration.Root
    $rootCanonical = ConvertTo-P3AccSingleQuotedLiteral $Configuration.RootCanonical
    $rootIdentity = ConvertTo-P3AccSingleQuotedLiteral $Configuration.RootIdentity
    $control = ConvertTo-P3AccSingleQuotedLiteral $Configuration.ControlRoot
    $controlCanonical = ConvertTo-P3AccSingleQuotedLiteral $Configuration.ControlRootCanonical
    $controlIdentity = ConvertTo-P3AccSingleQuotedLiteral $Configuration.ControlRootIdentity
    $result = ConvertTo-P3AccSingleQuotedLiteral $ResultPath
    $content = @"
Set-StrictMode -Version Latest
`$ErrorActionPreference = 'Stop'
Import-Module -Name $module -Force -ErrorAction Stop
`$configuration = [pscustomobject]@{ Root = $root; RootCanonical = $rootCanonical; RootIdentity = $rootIdentity; ControlRoot = $control; ControlRootCanonical = $controlCanonical; ControlRootIdentity = $controlIdentity }
`$identity = [pscustomobject]@{ ProcessId = $($AppState.Identity.ProcessId); StartedAtUtcTicks = $($AppState.Identity.StartedAtUtcTicks) }
try {
    if (-not (Test-P3AccProcessIdentity `$identity)) { throw 'P3ACC_CONTROLLER_VISUAL_FAILED' }
    `$process = [Diagnostics.Process]::GetProcessById([int]`$identity.ProcessId)
    `$appState = [pscustomobject]@{ Identity = `$identity; Process = `$process }
    `$capture = Save-P3AccSafeStatusCrop -Configuration `$configuration -AppState `$appState
    `$evidence = Wait-P3AccEvidenceAcknowledgement -Configuration `$configuration -Capture `$capture -AppState `$appState -TimeoutSeconds $EvidenceTimeoutSeconds
    `$closed = Send-P3AccWindowClose -AppState `$appState -TimeoutSeconds $CloseTimeoutSeconds
    `$process.Refresh()
    `$exitCodeZero = `$closed -and `$process.HasExited -and `$process.ExitCode -eq 0
    `$payload = [ordered]@{ schema = 'P3ACC-VISUAL/v1'; passed = [bool](`$closed -and `$exitCodeZero); sha256 = `$evidence.SHA256; width = [int]`$capture.Width; height = [int]`$capture.Height; nonUniform = [bool]`$capture.NonUniform; evidenceAcknowledged = [bool]`$evidence.Acknowledged; wmCloseSent = [bool]`$closed; appExitCodeZero = [bool]`$exitCodeZero }
} catch {
    `$payload = [ordered]@{ schema = 'P3ACC-VISUAL/v1'; passed = `$false; sha256 = ''; width = 0; height = 0; nonUniform = `$false; evidenceAcknowledged = `$false; wmCloseSent = `$false; appExitCodeZero = `$false }
}
`$json = `$payload | ConvertTo-Json -Compress
`$temporary = $result + '.tmp'
`$controlNow = [P3Acc.NativePath]::Inspect($control, `$true)
if (`$controlNow.Key -cne $controlIdentity -or -not [string]::Equals(`$controlNow.FinalPath, $controlCanonical, [StringComparison]::OrdinalIgnoreCase)) { throw 'P3ACC_CONTROLLER_VISUAL_FAILED' }
[IO.File]::WriteAllText(`$temporary, `$json, [Text.UTF8Encoding]::new(`$false))
`$controlNow = [P3Acc.NativePath]::Inspect($control, `$true)
`$temporaryNow = [P3Acc.NativePath]::Inspect(`$temporary, `$false)
if (`$controlNow.Key -cne $controlIdentity -or -not [string]::Equals(`$controlNow.FinalPath, $controlCanonical, [StringComparison]::OrdinalIgnoreCase) -or
    -not [string]::Equals([IO.Path]::GetDirectoryName(`$temporaryNow.FinalPath), `$controlNow.FinalPath, [StringComparison]::OrdinalIgnoreCase)) { throw 'P3ACC_CONTROLLER_VISUAL_FAILED' }
Move-Item -LiteralPath `$temporary -Destination $result -Force
`$controlNow = [P3Acc.NativePath]::Inspect($control, `$true)
`$resultNow = [P3Acc.NativePath]::Inspect($result, `$false)
if (`$controlNow.Key -cne $controlIdentity -or -not [string]::Equals(`$controlNow.FinalPath, $controlCanonical, [StringComparison]::OrdinalIgnoreCase) -or
    -not [string]::Equals([IO.Path]::GetDirectoryName(`$resultNow.FinalPath), `$controlNow.FinalPath, [StringComparison]::OrdinalIgnoreCase)) { throw 'P3ACC_CONTROLLER_VISUAL_FAILED' }
"@
    [void](Assert-P3AccControlRootStillOwned $Configuration)
    [IO.File]::WriteAllText($HelperPath, $content, [Text.UTF8Encoding]::new($false))
    [void](Assert-P3AccControlRootStillOwned $Configuration)
    [void](Assert-P3AccNoReparsePath -Path $HelperPath -Directory $false)
}

function Invoke-P3AccInteractiveVisualAcceptance {
    param($Configuration, $AppState, [int]$EvidenceTimeoutSeconds, [int]$CloseTimeoutSeconds)
    $helperPath = [IO.Path]::Combine($Configuration.ControlRoot, 'visual-helper.ps1')
    [void](Assert-P3AccControlRootStillOwned $Configuration)
    $resultPath = [IO.Path]::Combine($Configuration.ControlRoot, 'visual-result.json')
    Write-P3AccInteractiveVisualHelper -Configuration $Configuration -AppState $AppState -EvidenceTimeoutSeconds $EvidenceTimeoutSeconds -CloseTimeoutSeconds $CloseTimeoutSeconds -HelperPath $helperPath -ResultPath $resultPath
    $taskName = 'DouyinLive.P3ACC.Visual.' + [Guid]::NewGuid().ToString('N')
    $arguments = '-NoProfile -NonInteractive -WindowStyle Hidden -ExecutionPolicy Bypass -File "' + $helperPath + '"'
    try {
        $action = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument $arguments
        $principal = New-ScheduledTaskPrincipal -UserId ([Security.Principal.WindowsIdentity]::GetCurrent().Name) -LogonType Interactive -RunLevel Limited
        $settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit (New-TimeSpan -Seconds ($EvidenceTimeoutSeconds + $CloseTimeoutSeconds + 60)) -MultipleInstances IgnoreNew
        Register-ScheduledTask -TaskName $taskName -Action $action -Principal $principal -Settings $settings -Force | Out-Null
        Start-ScheduledTask -TaskName $taskName
        $deadline = [DateTime]::UtcNow.AddSeconds($EvidenceTimeoutSeconds + $CloseTimeoutSeconds + 60)
        $resultExists = $false
        while ([DateTime]::UtcNow -lt $deadline) {
            try { [void](Assert-P3AccControlRootStillOwned $Configuration) }
            catch { Throw-P3AccFailure 'P3ACC_CONTROLLER_VISUAL_FAILED' }
            $resultExists = Test-Path -LiteralPath $resultPath -PathType Leaf
            if ($resultExists) { break }
            if (Test-P3AccProcessIdentity $AppState.Identity) {
                try { [void](Register-P3AccCurrentDescendantIdentities -AppState $AppState) }
                catch {
                    if (Test-P3AccProcessIdentity $AppState.Identity) { throw }
                }
            }
            Start-Sleep -Milliseconds 250
        }
        if (Test-P3AccProcessIdentity $AppState.Identity) {
            [void](Register-P3AccCurrentDescendantIdentities -AppState $AppState)
        }
        if (-not $resultExists) { Throw-P3AccFailure 'P3ACC_CONTROLLER_TIMEOUT' }
        [void](Assert-P3AccControlRootStillOwned $Configuration)
        $resultInfo = Assert-P3AccNoReparsePath -Path $resultPath -Directory $false
        if ((Get-Item -LiteralPath $resultInfo.Path).Length -gt 8192) { Throw-P3AccFailure 'P3ACC_CONTROLLER_VISUAL_FAILED' }
        $visual = Get-Content -LiteralPath $resultInfo.Path -Raw -ErrorAction Stop | ConvertFrom-Json -ErrorAction Stop
        [void](Assert-P3AccControlRootStillOwned $Configuration)
        Assert-P3AccProperties $visual @('schema','passed','sha256','width','height','nonUniform','evidenceAcknowledged','wmCloseSent','appExitCodeZero')
        if ($visual.schema -cne 'P3ACC-VISUAL/v1' -or $visual.passed -isnot [bool] -or $visual.sha256 -isnot [string] -or
            -not (Test-P3AccInteger $visual.width) -or -not (Test-P3AccInteger $visual.height) -or
            $visual.nonUniform -isnot [bool] -or $visual.evidenceAcknowledged -isnot [bool] -or $visual.wmCloseSent -isnot [bool] -or $visual.appExitCodeZero -isnot [bool] -or
            -not $visual.passed -or $visual.sha256 -cnotmatch '^[0-9a-f]{64}$' -or [int]$visual.width -lt 300 -or [int]$visual.height -lt 120 -or
            -not $visual.nonUniform -or -not $visual.evidenceAcknowledged -or -not $visual.wmCloseSent -or -not $visual.appExitCodeZero) { Throw-P3AccFailure 'P3ACC_CONTROLLER_VISUAL_FAILED' }
        return $visual
    } finally {
        Stop-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
        Unregister-ScheduledTask -TaskName $taskName -Confirm:$false -ErrorAction SilentlyContinue
        $controlCleanupSafe = $true
        try { [void](Assert-P3AccControlRootStillOwned $Configuration) }
        catch { $controlCleanupSafe = $false }
        if ($controlCleanupSafe) {
            foreach ($ownedPath in @($helperPath, $resultPath)) {
                if (Test-Path -LiteralPath $ownedPath) {
                    try {
                        [void](Assert-P3AccNoReparsePath -Path $ownedPath -Directory $false)
                        Remove-Item -LiteralPath $ownedPath -Force -ErrorAction Stop
                    } catch { $controlCleanupSafe = $false; break }
                }
            }
        }
        if (-not $controlCleanupSafe -or $null -ne (Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue) -or
            (Test-Path -LiteralPath $helperPath) -or (Test-Path -LiteralPath $resultPath)) {
            Throw-P3AccFailure 'P3ACC_CONTROLLER_VISUAL_FAILED'
        }
    }
}

function Test-P3AccDatabaseAfterClose {
    param([Parameter(Mandatory)]$Configuration)
    try {
        [void](Assert-P3AccControlRootStillOwned $Configuration)
        Assert-P3AccRootStillOwned $Configuration
        [void](Assert-P3AccNoReparsePath -Path $Configuration.DatabasePath -Directory $false)
    }
    catch { Throw-P3AccFailure 'P3ACC_CONTROLLER_FINAL_INVALID' }
    $process = $null
    $stdoutTask = $null
    $stderrTask = $null
    try {
        [void](Assert-P3AccControlRootStillOwned $Configuration)
        $startInfo = [Diagnostics.ProcessStartInfo]::new()
        $startInfo.FileName = $Configuration.SQLiteExecutable
        $startInfo.Arguments = '-readonly "' + $Configuration.DatabasePath + '" "PRAGMA quick_check;"'
        $startInfo.WorkingDirectory = [IO.Path]::GetDirectoryName($Configuration.SQLiteExecutable)
        $startInfo.UseShellExecute = $false
        $startInfo.CreateNoWindow = $true
        $startInfo.RedirectStandardOutput = $true
        $startInfo.RedirectStandardError = $true
        $process = [Diagnostics.Process]::new()
        $process.StartInfo = $startInfo
        if (-not $process.Start()) { Throw-P3AccFailure 'P3ACC_CONTROLLER_FINAL_INVALID' }
        $stdoutTask = $process.StandardOutput.ReadToEndAsync()
        $stderrTask = $process.StandardError.ReadToEndAsync()
        if (-not $process.WaitForExit(30000)) {
            try { $process.Kill(); [void]$process.WaitForExit(5000) } catch { }
            Throw-P3AccFailure 'P3ACC_CONTROLLER_FINAL_INVALID'
        }
        $process.WaitForExit()
        $rawOut = [string]$stdoutTask.GetAwaiter().GetResult()
        $rawErr = [string]$stderrTask.GetAwaiter().GetResult()
        if ($process.ExitCode -ne 0 -or $rawOut.Length -gt 1024 -or $rawErr.Length -gt 1024) { Throw-P3AccFailure 'P3ACC_CONTROLLER_FINAL_INVALID' }
        if ($null -eq $rawOut -or ([string]$rawOut) -cnotmatch '^ok\r?\n$' -or -not [string]::IsNullOrEmpty([string]$rawErr)) { Throw-P3AccFailure 'P3ACC_CONTROLLER_FINAL_INVALID' }
    } finally {
        if ($null -ne $process) { try { $process.Dispose() } catch { } }
    }
    try {
        Assert-P3AccRootStillOwned $Configuration
        [void](Assert-P3AccNoReparsePath -Path $Configuration.DatabasePath -Directory $false)
        $stream = [IO.File]::Open($Configuration.DatabasePath, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::None)
        $stream.Dispose()
    } catch { Throw-P3AccFailure 'P3ACC_CONTROLLER_FINAL_INVALID' }
    return [pscustomobject]@{ QuickCheckPassed = $true; Unlocked = $true }
}

function Send-P3AccWindowClose {
    param([Parameter(Mandatory)]$AppState, [int]$TimeoutSeconds)
    if (-not (Test-P3AccProcessIdentity $AppState.Identity)) { return $true }
    $sent = [P3Acc.NativeWindow]::SendClose([uint32]$AppState.Identity.ProcessId)
    if (-not $sent) { return $false }
    try { $exited = $AppState.Process.WaitForExit($TimeoutSeconds * 1000) }
    catch { $exited = $false }
    return $exited -and -not (Test-P3AccProcessIdentity $AppState.Identity)
}

function Get-P3AccCleanupIdentityState {
    param([Parameter(Mandatory)]$Identity)
    if ($null -eq $Identity -or
        $Identity.PSObject.Properties.Name -cnotcontains 'ProcessId' -or
        $Identity.PSObject.Properties.Name -cnotcontains 'StartedAtUtcTicks' -or
        -not (Test-P3AccInteger $Identity.ProcessId) -or
        -not (Test-P3AccInteger $Identity.StartedAtUtcTicks) -or
        [int64]$Identity.ProcessId -lt 1 -or [int64]$Identity.ProcessId -gt [int]::MaxValue -or
        [int64]$Identity.StartedAtUtcTicks -lt 1) {
        Throw-P3AccFailure 'P3ACC_CONTROLLER_CLEANUP_FAILED'
    }
    for ($attempt = 0; $attempt -lt 2; $attempt++) {
        $process = $null
        try {
            $process = [Diagnostics.Process]::GetProcessById([int]$Identity.ProcessId)
            if ($process.HasExited) { return 'Absent' }
            $ticks = $process.StartTime.ToUniversalTime().Ticks
            if ($process.HasExited) { return 'Absent' }
            if ($ticks -ne [int64]$Identity.StartedAtUtcTicks) { return 'Reused' }
            return 'Alive'
        } catch {
            if (Test-P3AccExceptionChainType $_ ([ArgumentException])) { return 'Absent' }
            if ((Test-P3AccExceptionChainType $_ ([InvalidOperationException])) -and $attempt -eq 0) {
                Start-Sleep -Milliseconds 10
                continue
            }
            Throw-P3AccFailure 'P3ACC_CONTROLLER_CLEANUP_FAILED'
        } finally {
            if ($null -ne $process) { try { $process.Dispose() } catch { } }
        }
    }
    Throw-P3AccFailure 'P3ACC_CONTROLLER_CLEANUP_FAILED'
}

function Stop-P3AccIdentity {
    param($Identity, [int]$WaitSeconds = 10)
    if ($null -eq $Identity) { return $true }
    if ($WaitSeconds -lt 1 -or $WaitSeconds -gt 30) { return $false }
    try { $state = Get-P3AccCleanupIdentityState $Identity }
    catch { return $false }
    if ($state -ceq 'Absent') { return $true }
    if ($state -cne 'Alive') { return $false }
    $process = $null
    try {
        $process = [Diagnostics.Process]::GetProcessById([int]$Identity.ProcessId)
        if ($process.HasExited) { return $true }
        if ($process.StartTime.ToUniversalTime().Ticks -ne [int64]$Identity.StartedAtUtcTicks -or $process.HasExited) { return $false }
        $process.Kill()
        if (-not $process.WaitForExit($WaitSeconds * 1000)) { return $false }
    } catch {
        if (-not (Test-P3AccExceptionChainType $_ ([ArgumentException])) -and
            -not (Test-P3AccExceptionChainType $_ ([InvalidOperationException]))) { return $false }
    }
    finally { if ($null -ne $process) { try { $process.Dispose() } catch { } } }
    try { return (Get-P3AccCleanupIdentityState $Identity) -ceq 'Absent' }
    catch { return $false }
}

function Stop-P3AccAppTree {
    param($AppState)
    if ($null -eq $AppState) { return $true }
    if ($AppState.PSObject.Properties.Name -cnotcontains 'JobState' -or $null -eq $AppState.JobState) { return $false }
    $jobState = $AppState.JobState
    try {
        if ([bool]$jobState.Closed) {
            return [bool]$jobState.GracefulExitConfirmed -or [bool]$jobState.ForcedEmptyConfirmed
        }
        $launcherStopped = $true
        if ($AppState.PSObject.Properties.Name -ccontains 'LauncherIdentity') {
            $launcherStopped = Stop-P3AccLauncherOutsideJob -JobState $jobState -LauncherIdentity $AppState.LauncherIdentity
        }
        $jobStopped = Stop-P3AccJobState -JobState $jobState -TimeoutSeconds 30
        return $launcherStopped -and $jobStopped
    } finally {
        if ($AppState.PSObject.Properties.Name -ccontains 'Process' -and $null -ne $AppState.Process) {
            try { $AppState.Process.Dispose() } catch { }
        }
    }
}

function Assert-P3AccTreeNoReparse {
    param([Parameter(Mandatory)][string]$Root)
    $queue = [Collections.Queue]::new()
    $queue.Enqueue($Root)
    $count = 0
    while ($queue.Count -gt 0) {
        if ($queue.Count -gt 200000) { Throw-P3AccFailure 'P3ACC_CONTROLLER_CLEANUP_FAILED' }
        $directory = [string]$queue.Dequeue()
        foreach ($entry in Get-ChildItem -LiteralPath $directory -Force -ErrorAction Stop) {
            $count++
            if ($count -gt 200000 -or ($entry.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
                Throw-P3AccFailure 'P3ACC_CONTROLLER_CLEANUP_FAILED'
            }
            if ($entry.PSIsContainer) { $queue.Enqueue($entry.FullName) }
        }
    }
}

function New-P3AccReportMetric {
    return [ordered]@{ baseline = 0; peak = 0; latest = 0; delta = 0; latterHalfDelta = 0; latterHalfTrend = 'INSUFFICIENT' }
}

function New-P3AccControllerReport {
    return [ordered]@{
        schema = $script:P3AccReportSchema; passed = $false; code = 'P3ACC_CONTROLLER_INTERNAL_ERROR'
        rootValidated = $false; secretValidated = $false; secretRemoved = $false
        relayDynamicPort = $false; relayBaselineProbe = $false; relayFaultProven = $false; relaySamePortRestored = $false
        appInteractiveLaunch = $false; snapshotContract = $false; stableWindowObserved = $false
        uiBaselineObserved = $false; crashRecoveryObserved = $false; networkFaultArmedObserved = $false
        networkRecoveryObserved = $false; finalizationObserved = $false
        topology = [ordered]@{ sampleCount = 0; beforeFaultSampleCount = 0; afterRecoverySampleCount = 0; appOnlyRelay = $false; ffmpegOnlyRelay = $false; relayOnlyUpstream = $false; noUdpBypass = $false }
        visual = [ordered]@{ safeCropCaptured = $false; sha256 = ''; width = 0; height = 0; nonUniform = $false; evidenceAcknowledged = $false; wmCloseSent = $false; appExitCodeZero = $false; naturalAppTreeExited = $false }
        database = [ordered]@{ quickCheckPassed = $false; unlocked = $false }
        metrics = [ordered]@{
            resources = [ordered]@{
                sampleCount = 0; windowMs = 0; ownedProcessTreeCPUAvgPct = 0.0
                ownedProcessTreeLatterHalfCPUAvgPct = 0.0; cpuTrend = 'INSUFFICIENT'
                databaseWalObserved = $false; diskIoObserved = $false; eventQueueObserved = $false
                averageProcessReadBytesPerSecond = 0.0; averageProcessWriteBytesPerSecond = 0.0
                latterHalfProcessReadBytesPerSecond = 0.0; latterHalfProcessWriteBytesPerSecond = 0.0
                averageDiskWriteBytesPerSecond = 0.0; latterHalfDiskWriteBytesPerSecond = 0.0
                processCount = New-P3AccReportMetric; workingSet = New-P3AccReportMetric
                privateBytes = New-P3AccReportMetric; threads = New-P3AccReportMetric
                handles = New-P3AccReportMetric; goroutines = New-P3AccReportMetric
                heapAlloc = New-P3AccReportMetric; heapInUse = New-P3AccReportMetric
                system = New-P3AccReportMetric
                databaseWalBytes = New-P3AccReportMetric; processReadBytes = New-P3AccReportMetric
                processWriteBytes = New-P3AccReportMetric; dataRootPhysicalBytes = New-P3AccReportMetric
                eventQueueCount = New-P3AccReportMetric
                eventQueueItems = New-P3AccReportMetric; eventQueueBytes = New-P3AccReportMetric
                eventQueueItemCapacity = New-P3AccReportMetric; eventQueueByteCapacity = New-P3AccReportMetric
            }
            uiLatency = [ordered]@{ sampleCount = 0; p95Ms = 0; maxMs = 0 }
            lineage = [ordered]@{
                runtimeAttemptCount = 0; progressRestartCount = 0; mediaAttemptCount = 0
                committedAttemptCount = 0; segmentCount = 0; artifactCount = 0
                processCrashGapCount = 0; recordingRestartGapCount = 0; messageDisconnectGapCount = 0
            }
        }
        cleanup = [ordered]@{ taskRemoved = $false; appStopped = $false; relayStopped = $false; secretRemoved = $false; ephemeralRootRemoved = $false; controlRootRemoved = $false; zeroResidual = $false }
    }
}

function Assert-P3AccReportProperties {
    param($Value, [string[]]$Expected)
    if ($null -eq $Value -or ($Value -isnot [Collections.IDictionary] -and $Value -isnot [pscustomobject])) { Throw-P3AccFailure 'P3ACC_CONTROLLER_INTERNAL_ERROR' }
    if ($Value -is [Collections.IDictionary]) {
        $actual = @($Value.Keys | ForEach-Object { [string]$_ } | Sort-Object)
    } else {
        $actual = @($Value.PSObject.Properties | ForEach-Object { $_.Name } | Sort-Object)
    }
    $wanted = @($Expected | Sort-Object)
    if ($actual.Count -ne $wanted.Count -or @(Compare-Object -ReferenceObject $wanted -DifferenceObject $actual).Count -ne 0) { Throw-P3AccFailure 'P3ACC_CONTROLLER_INTERNAL_ERROR' }
}

function Assert-P3AccReportMetric {
    param($Metric)
    Assert-P3AccReportProperties $Metric @('baseline','peak','latest','delta','latterHalfDelta','latterHalfTrend')
    foreach ($name in @('baseline','peak','latest')) {
        if (-not (Test-P3AccInteger $Metric.$name) -or [int64]$Metric.$name -lt 0) { Throw-P3AccFailure 'P3ACC_CONTROLLER_INTERNAL_ERROR' }
    }
    if (-not (Test-P3AccInteger $Metric.delta) -or -not (Test-P3AccInteger $Metric.latterHalfDelta) -or $Metric.latterHalfTrend -isnot [string] -or
        @('INSUFFICIENT','STABLE','RISING','FALLING') -cnotcontains $Metric.latterHalfTrend -or
        [int64]$Metric.peak -lt [int64]$Metric.baseline -or [int64]$Metric.peak -lt [int64]$Metric.latest -or
        [int64]$Metric.delta -ne ([int64]$Metric.latest - [int64]$Metric.baseline)) { Throw-P3AccFailure 'P3ACC_CONTROLLER_INTERNAL_ERROR' }
}

function Assert-P3AccControllerReport {
    param([Parameter(Mandatory)]$Report)
    $top = @('schema','passed','code','rootValidated','secretValidated','secretRemoved','relayDynamicPort','relayBaselineProbe','relayFaultProven','relaySamePortRestored','appInteractiveLaunch','snapshotContract','stableWindowObserved','uiBaselineObserved','crashRecoveryObserved','networkFaultArmedObserved','networkRecoveryObserved','finalizationObserved','topology','visual','database','metrics','cleanup')
    Assert-P3AccReportProperties $Report $top
    if ($Report.schema -cne $script:P3AccReportSchema -or $Report.passed -isnot [bool] -or $Report.code -isnot [string]) { Throw-P3AccFailure 'P3ACC_CONTROLLER_INTERNAL_ERROR' }
    $codes = @('OK','P3ACC_CONTROLLER_PLATFORM_INVALID','P3ACC_CONTROLLER_CONFIG_INVALID','P3ACC_CONTROLLER_ROOT_INVALID','P3ACC_CONTROLLER_SECRET_INVALID','P3ACC_CONTROLLER_RELAY_FAILED','P3ACC_CONTROLLER_APP_LAUNCH_FAILED','P3ACC_CONTROLLER_SNAPSHOT_INVALID','P3ACC_CONTROLLER_TIMEOUT','P3ACC_CONTROLLER_TOPOLOGY_INVALID','P3ACC_CONTROLLER_PROBE_FAILED','P3ACC_CONTROLLER_FINAL_INVALID','P3ACC_CONTROLLER_VISUAL_FAILED','P3ACC_CONTROLLER_CLEANUP_FAILED','P3ACC_CONTROLLER_INTERNAL_ERROR')
    if ($codes -cnotcontains $Report.code) { Throw-P3AccFailure 'P3ACC_CONTROLLER_INTERNAL_ERROR' }
    $evidenceNames = @('rootValidated','secretValidated','secretRemoved','relayDynamicPort','relayBaselineProbe','relayFaultProven','relaySamePortRestored','appInteractiveLaunch','snapshotContract','stableWindowObserved','uiBaselineObserved','crashRecoveryObserved','networkFaultArmedObserved','networkRecoveryObserved','finalizationObserved')
    foreach ($name in $evidenceNames) { if ($Report.$name -isnot [bool]) { Throw-P3AccFailure 'P3ACC_CONTROLLER_INTERNAL_ERROR' } }
    Assert-P3AccReportProperties $Report.topology @('sampleCount','beforeFaultSampleCount','afterRecoverySampleCount','appOnlyRelay','ffmpegOnlyRelay','relayOnlyUpstream','noUdpBypass')
    foreach ($name in @('sampleCount','beforeFaultSampleCount','afterRecoverySampleCount')) { if (-not (Test-P3AccInteger $Report.topology.$name) -or [int]$Report.topology.$name -lt 0) { Throw-P3AccFailure 'P3ACC_CONTROLLER_INTERNAL_ERROR' } }
    foreach ($name in @('appOnlyRelay','ffmpegOnlyRelay','relayOnlyUpstream','noUdpBypass')) { if ($Report.topology.$name -isnot [bool]) { Throw-P3AccFailure 'P3ACC_CONTROLLER_INTERNAL_ERROR' } }
    Assert-P3AccReportProperties $Report.visual @('safeCropCaptured','sha256','width','height','nonUniform','evidenceAcknowledged','wmCloseSent','appExitCodeZero','naturalAppTreeExited')
    foreach ($name in @('safeCropCaptured','nonUniform','evidenceAcknowledged','wmCloseSent','appExitCodeZero','naturalAppTreeExited')) { if ($Report.visual.$name -isnot [bool]) { Throw-P3AccFailure 'P3ACC_CONTROLLER_INTERNAL_ERROR' } }
    if ($Report.visual.sha256 -isnot [string]) { Throw-P3AccFailure 'P3ACC_CONTROLLER_INTERNAL_ERROR' }
    foreach ($name in @('width','height')) { if (-not (Test-P3AccInteger $Report.visual.$name) -or [int]$Report.visual.$name -lt 0) { Throw-P3AccFailure 'P3ACC_CONTROLLER_INTERNAL_ERROR' } }
    Assert-P3AccReportProperties $Report.database @('quickCheckPassed','unlocked')
    foreach ($name in @('quickCheckPassed','unlocked')) { if ($Report.database.$name -isnot [bool]) { Throw-P3AccFailure 'P3ACC_CONTROLLER_INTERNAL_ERROR' } }
    Assert-P3AccReportProperties $Report.metrics @('resources','uiLatency','lineage')
    $stableResourceMetricNames = @('processCount','workingSet','privateBytes','threads','handles','goroutines','heapAlloc','heapInUse','system')
    $resourceMetricNames = @($stableResourceMetricNames) + @('databaseWalBytes','processReadBytes','processWriteBytes','dataRootPhysicalBytes','eventQueueCount','eventQueueItems','eventQueueBytes','eventQueueItemCapacity','eventQueueByteCapacity')
    Assert-P3AccReportProperties $Report.metrics.resources @('sampleCount','windowMs','ownedProcessTreeCPUAvgPct','ownedProcessTreeLatterHalfCPUAvgPct','cpuTrend','databaseWalObserved','diskIoObserved','eventQueueObserved','averageProcessReadBytesPerSecond','averageProcessWriteBytesPerSecond','latterHalfProcessReadBytesPerSecond','latterHalfProcessWriteBytesPerSecond','averageDiskWriteBytesPerSecond','latterHalfDiskWriteBytesPerSecond','processCount','workingSet','privateBytes','threads','handles','goroutines','heapAlloc','heapInUse','system','databaseWalBytes','processReadBytes','processWriteBytes','dataRootPhysicalBytes','eventQueueCount','eventQueueItems','eventQueueBytes','eventQueueItemCapacity','eventQueueByteCapacity')
    foreach ($name in @('sampleCount','windowMs')) {
        if (-not (Test-P3AccInteger $Report.metrics.resources.$name) -or [int64]$Report.metrics.resources.$name -lt 0) { Throw-P3AccFailure 'P3ACC_CONTROLLER_INTERNAL_ERROR' }
    }
    foreach ($name in @('databaseWalObserved','diskIoObserved','eventQueueObserved')) {
        if ($Report.metrics.resources.$name -isnot [bool]) { Throw-P3AccFailure 'P3ACC_CONTROLLER_INTERNAL_ERROR' }
    }
    foreach ($name in @('ownedProcessTreeCPUAvgPct','ownedProcessTreeLatterHalfCPUAvgPct')) {
        if ($Report.metrics.resources.$name -isnot [ValueType] -or [double]$Report.metrics.resources.$name -lt 0 -or [double]$Report.metrics.resources.$name -gt 1000 -or [double]::IsNaN([double]$Report.metrics.resources.$name) -or [double]::IsInfinity([double]$Report.metrics.resources.$name)) { Throw-P3AccFailure 'P3ACC_CONTROLLER_INTERNAL_ERROR' }
    }
    foreach ($name in @('averageProcessReadBytesPerSecond','averageProcessWriteBytesPerSecond','latterHalfProcessReadBytesPerSecond','latterHalfProcessWriteBytesPerSecond','averageDiskWriteBytesPerSecond','latterHalfDiskWriteBytesPerSecond')) {
        if ($Report.metrics.resources.$name -isnot [ValueType] -or [double]$Report.metrics.resources.$name -lt 0 -or [double]::IsNaN([double]$Report.metrics.resources.$name) -or [double]::IsInfinity([double]$Report.metrics.resources.$name)) { Throw-P3AccFailure 'P3ACC_CONTROLLER_INTERNAL_ERROR' }
    }
    if ($Report.metrics.resources.cpuTrend -isnot [string] -or @('INSUFFICIENT','STABLE','RISING','FALLING') -cnotcontains $Report.metrics.resources.cpuTrend) { Throw-P3AccFailure 'P3ACC_CONTROLLER_INTERNAL_ERROR' }
    foreach ($name in $resourceMetricNames) { Assert-P3AccReportMetric $Report.metrics.resources.$name }
    Assert-P3AccReportProperties $Report.metrics.uiLatency @('sampleCount','p95Ms','maxMs')
    foreach ($name in @('sampleCount','p95Ms','maxMs')) {
        if (-not (Test-P3AccInteger $Report.metrics.uiLatency.$name) -or [int64]$Report.metrics.uiLatency.$name -lt 0 -or [int64]$Report.metrics.uiLatency.$name -gt 86400000) { Throw-P3AccFailure 'P3ACC_CONTROLLER_INTERNAL_ERROR' }
    }
    $lineageNames = @('runtimeAttemptCount','progressRestartCount','mediaAttemptCount','committedAttemptCount','segmentCount','artifactCount','processCrashGapCount','recordingRestartGapCount','messageDisconnectGapCount')
    Assert-P3AccReportProperties $Report.metrics.lineage $lineageNames
    foreach ($name in $lineageNames) {
        if (-not (Test-P3AccInteger $Report.metrics.lineage.$name) -or [int64]$Report.metrics.lineage.$name -lt 0 -or [int64]$Report.metrics.lineage.$name -gt 1000000) { Throw-P3AccFailure 'P3ACC_CONTROLLER_INTERNAL_ERROR' }
    }
    $cleanupNames = @('taskRemoved','appStopped','relayStopped','secretRemoved','ephemeralRootRemoved','controlRootRemoved','zeroResidual')
    Assert-P3AccReportProperties $Report.cleanup $cleanupNames
    foreach ($name in $cleanupNames) { if ($Report.cleanup.$name -isnot [bool]) { Throw-P3AccFailure 'P3ACC_CONTROLLER_INTERNAL_ERROR' } }
    if ($Report.passed) {
        if ($Report.code -cne 'OK') { Throw-P3AccFailure 'P3ACC_CONTROLLER_INTERNAL_ERROR' }
        foreach ($name in $evidenceNames) { if (-not $Report.$name) { Throw-P3AccFailure 'P3ACC_CONTROLLER_INTERNAL_ERROR' } }
        if ([int]$Report.topology.beforeFaultSampleCount -lt 3 -or [int]$Report.topology.afterRecoverySampleCount -lt 3 -or
            [int]$Report.topology.sampleCount -ne [int]$Report.topology.beforeFaultSampleCount + [int]$Report.topology.afterRecoverySampleCount -or
            -not $Report.topology.appOnlyRelay -or -not $Report.topology.ffmpegOnlyRelay -or -not $Report.topology.relayOnlyUpstream -or -not $Report.topology.noUdpBypass -or
            -not $Report.visual.safeCropCaptured -or $Report.visual.sha256 -cnotmatch '^[0-9a-f]{64}$' -or
            [int]$Report.visual.width -lt 300 -or [int]$Report.visual.width -gt 420 -or [int]$Report.visual.height -ne 180 -or
            -not $Report.visual.nonUniform -or -not $Report.visual.evidenceAcknowledged -or -not $Report.visual.wmCloseSent -or -not $Report.visual.appExitCodeZero -or -not $Report.visual.naturalAppTreeExited -or
            -not $Report.database.quickCheckPassed -or -not $Report.database.unlocked) { Throw-P3AccFailure 'P3ACC_CONTROLLER_INTERNAL_ERROR' }
        if ([int]$Report.metrics.resources.sampleCount -lt 30 -or [int]$Report.metrics.resources.sampleCount -gt 128 -or
            [int64]$Report.metrics.resources.windowMs -lt 600000 -or [double]$Report.metrics.resources.ownedProcessTreeCPUAvgPct -ge 10 -or
            [double]$Report.metrics.resources.ownedProcessTreeLatterHalfCPUAvgPct -ge 10 -or @('STABLE','FALLING') -cnotcontains $Report.metrics.resources.cpuTrend -or
            [int]$Report.metrics.uiLatency.sampleCount -lt 1 -or [int]$Report.metrics.uiLatency.p95Ms -ge 1000 -or
            [int]$Report.metrics.uiLatency.p95Ms -gt [int]$Report.metrics.uiLatency.maxMs -or
            [int]$Report.metrics.lineage.runtimeAttemptCount -lt 3 -or [int]$Report.metrics.lineage.progressRestartCount -lt 2 -or
            [int]$Report.metrics.lineage.mediaAttemptCount -lt 3 -or [int]$Report.metrics.lineage.committedAttemptCount -lt 3 -or
            [int]$Report.metrics.lineage.segmentCount -lt 1 -or [int]$Report.metrics.lineage.processCrashGapCount -lt 1 -or
            [int]$Report.metrics.lineage.recordingRestartGapCount -lt 1 -or [int]$Report.metrics.lineage.messageDisconnectGapCount -lt 1) { Throw-P3AccFailure 'P3ACC_CONTROLLER_INTERNAL_ERROR' }
        if (-not (Test-P3AccResourceObservationInvariants $Report.metrics.resources -RequireObserved)) { Throw-P3AccFailure 'P3ACC_CONTROLLER_INTERNAL_ERROR' }
        foreach ($name in $stableResourceMetricNames) {
            if (@('STABLE','FALLING') -cnotcontains $Report.metrics.resources.$name.latterHalfTrend) { Throw-P3AccFailure 'P3ACC_CONTROLLER_INTERNAL_ERROR' }
        }
        foreach ($name in $cleanupNames) { if (-not $Report.cleanup.$name) { Throw-P3AccFailure 'P3ACC_CONTROLLER_INTERNAL_ERROR' } }
    } elseif ($Report.code -ceq 'OK') { Throw-P3AccFailure 'P3ACC_CONTROLLER_INTERNAL_ERROR' }
    return $true
}

function Complete-P3AccControllerCleanup {
    param($Configuration, $AppState, $RelayStates, $Report)
    if ($null -eq $Configuration) {
        $noOwnedState = $null -eq $AppState -and @($RelayStates).Count -eq 0
        $Report.cleanup.taskRemoved = [bool]$noOwnedState
        $Report.cleanup.appStopped = [bool]$noOwnedState
        $Report.cleanup.relayStopped = [bool]$noOwnedState
        $Report.cleanup.secretRemoved = [bool]$noOwnedState
        $Report.cleanup.ephemeralRootRemoved = [bool]$noOwnedState
        $Report.cleanup.controlRootRemoved = [bool]$noOwnedState
        $Report.cleanup.zeroResidual = [bool]$noOwnedState
        if ($noOwnedState) { return $true }
        return $false
    }

    $taskRemoved = $true
    if ($null -ne $AppState -and -not $AppState.TaskRemoved) {
        try {
            Stop-ScheduledTask -TaskName $AppState.TaskName -ErrorAction SilentlyContinue
            Unregister-ScheduledTask -TaskName $AppState.TaskName -Confirm:$false -ErrorAction SilentlyContinue
            $taskRemoved = $null -eq (Get-ScheduledTask -TaskName $AppState.TaskName -ErrorAction SilentlyContinue)
        } catch { $taskRemoved = $false }
    }
    $appStopped = Stop-P3AccAppTree $AppState
    $relayStopped = $true
    if ($null -ne $Configuration -and $Configuration.AppLaunchCleanupUncertain) {
        $taskRemoved = $false; $appStopped = $false
    }
    foreach ($relay in @($RelayStates)) {
        if (-not (Stop-P3AccExactProcess $relay)) { $relayStopped = $false }
        if ($null -ne $relay -and $relay.PSObject.Properties.Name -contains 'Process') { try { $relay.Process.Dispose() } catch { } }
    }
    $secretRemoved = $false
    if ($null -ne $Configuration -and $Configuration.RelayCleanupUncertain) { $relayStopped = $false }
    if ($null -ne $Configuration) {
        if (Test-Path -LiteralPath $Configuration.SecretPath -PathType Leaf) {
            try {
                $secretNow = Assert-P3AccNoReparsePath -Path $Configuration.SecretPath -Directory $false
                if ($secretNow.Identity -cne $Configuration.SecretIdentity) { Throw-P3AccFailure 'P3ACC_CONTROLLER_CLEANUP_FAILED' }
                Remove-Item -LiteralPath $Configuration.SecretPath -Force -ErrorAction Stop
            } catch { }
        }
        $secretRemoved = -not (Test-Path -LiteralPath $Configuration.SecretPath)
    }
    $rootRemoved = $false
    if ($null -ne $Configuration -and (Test-Path -LiteralPath $Configuration.Root -PathType Container)) {
        try {
            Assert-P3AccRootStillOwned $Configuration
            Assert-P3AccTreeNoReparse $Configuration.Root
            Remove-Item -LiteralPath $Configuration.Root -Recurse -Force -ErrorAction Stop
            $rootRemoved = -not (Test-Path -LiteralPath $Configuration.Root)
        } catch { $rootRemoved = $false }
    }
    $controlRootRemoved = $false
    if ($null -ne $Configuration -and (Test-Path -LiteralPath $Configuration.ControlRoot -PathType Container)) {
        try {
            $controlNow = Assert-P3AccNoReparsePath -Path $Configuration.ControlRoot -Directory $true
            if ($controlNow.Identity -cne $Configuration.ControlRootIdentity -or -not [string]::Equals($controlNow.Canonical, $Configuration.ControlRootCanonical, [StringComparison]::OrdinalIgnoreCase)) { Throw-P3AccFailure 'P3ACC_CONTROLLER_CLEANUP_FAILED' }
            Assert-P3AccTreeNoReparse $Configuration.ControlRoot
            Remove-Item -LiteralPath $Configuration.ControlRoot -Recurse -Force -ErrorAction Stop
            $controlRootRemoved = -not (Test-Path -LiteralPath $Configuration.ControlRoot)
        } catch { $controlRootRemoved = $false }
    }
    $zero = $taskRemoved -and $appStopped -and $relayStopped -and $secretRemoved -and $rootRemoved -and $controlRootRemoved
    $Report.cleanup.taskRemoved = [bool]$taskRemoved
    $Report.cleanup.appStopped = [bool]$appStopped
    $Report.cleanup.relayStopped = [bool]$relayStopped
    $Report.cleanup.secretRemoved = [bool]$secretRemoved
    $Report.cleanup.ephemeralRootRemoved = [bool]$rootRemoved
    $Report.cleanup.controlRootRemoved = [bool]$controlRootRemoved
    $Report.cleanup.zeroResidual = [bool]$zero
    $Report.secretRemoved = [bool]$secretRemoved
    return $zero
}

Export-ModuleMember -Function @(
    'Get-P3AccFailureCode','New-P3AccControllerConfiguration','Assert-P3AccRunRoot',
    'Assert-P3AccSecretFile','ConvertFrom-P3AccLoopbackEndpoint','Read-P3AccRelayAnnouncementText',
    'Assert-P3AccSnapshotContract','Read-P3AccSnapshot','Start-P3AccRelay','Stop-P3AccExactProcess',
    'Test-P3AccRelayProbe','Start-P3AccInteractiveApp','Get-P3AccTopologySample',
    'New-P3AccTopologyTracker','Reset-P3AccTopologyPhase','Add-P3AccTopologySample',
    'Test-P3AccTopologySnapshotEligible','Test-P3AccTopologyFenceStable','Test-P3AccTopologyPhaseReady',
    'Copy-P3AccTopologyTrackerToReport','Test-P3AccPreFaultReady',
    'Test-P3AccFaultObserved','Test-P3AccNetworkRecovered','Test-P3AccFinalContract',
    'Wait-P3AccSnapshot','Test-P3AccProcessIdentity','Save-P3AccSafeStatusCrop',
    'Register-P3AccCurrentDescendantIdentities','Wait-P3AccNaturalAppTreeExit',
    'Wait-P3AccEvidenceAcknowledgement','Invoke-P3AccInteractiveVisualAcceptance',
    'Send-P3AccWindowClose','Test-P3AccDatabaseAfterClose',
    'New-P3AccControllerReport','Assert-P3AccControllerReport','Complete-P3AccControllerCleanup'
)
