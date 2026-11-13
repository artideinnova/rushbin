//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// On Windows the drives are the letters (E:\, F:\ …) except the system one. A drive is known by the same
// identity the Mac gives it, so it is one drive whichever computer catalogs it: for FAT and exFAT the UUID
// macOS derives from the file system's serial number (fsUUID), for NTFS on a GPT disk the partition's GUID.

func driveRoots() []string {
	sys := strings.ToUpper(os.Getenv("SystemDrive")) // "C:"
	mask, _ := windows.GetLogicalDrives()
	var out []string
	for i := 2; i < 26; i++ { // A: and B: are floppies
		if mask&(1<<i) != 0 {
			if l := string(rune('A'+i)) + ":"; l != sys {
				out = append(out, l+`\`)
			}
		}
	}
	return out
}

func identifyWindows(vol string) (Device, error) {
	d := Device{Mount: vol}
	root := filepath.VolumeName(vol) + `\`
	p, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return d, err
	}
	switch windows.GetDriveType(p) {
	case windows.DRIVE_FIXED, windows.DRIVE_REMOVABLE:
	default: // network shares, optical discs, RAM disks: never cataloged as drives
		return d, nil
	}
	var name, fsName [windows.MAX_PATH + 1]uint16
	var serial uint32
	if err := windows.GetVolumeInformation(p, &name[0], uint32(len(name)), &serial, nil, nil, &fsName[0], uint32(len(fsName))); err != nil {
		return d, fmt.Errorf("volume information %s: %w", root, err)
	}
	var free, total, totalFree uint64
	windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree)
	d.FS = windows.UTF16ToString(fsName[:])
	d.UUID = volumeID(filepath.VolumeName(vol), d.FS, serial)
	d.Name = windows.UTF16ToString(name[:])
	if d.Name == "" {
		d.Name = strings.TrimSuffix(root, `\`)
	}
	d.Size, d.Free = int64(total), int64(free)
	return d, nil
}

// partitionInfo is PARTITION_INFORMATION_EX (winioctl.h), GPT side of the union.
type partitionInfo struct {
	Style            uint32 // 0 MBR, 1 GPT
	_                uint32
	Start, Length    int64
	Number           uint32
	Rewrite, Service bool
	_                [2]byte
	GptType, GptID   windows.GUID
	_                [80]byte
}

const ioctlDiskGetPartitionInfoEx = 0x00070048 // FILE_ANY_ACCESS

var getDiskFreeSpace = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetDiskFreeSpaceW")

// volumeID asks the volume, opened without read access (which would need an administrator), for its partition.
// NTFS on an MBR disk, which the Mac cannot identify, keeps WIN-serial: its serial number's low 32 bits.
func volumeID(letter, fsName string, serial uint32) string {
	fallback := fmt.Sprintf("WIN-%08X", serial)
	p, _ := windows.UTF16PtrFromString(`\\.\` + letter)
	h, err := windows.CreateFile(p, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return fallback
	}
	defer windows.CloseHandle(h)
	var part partitionInfo
	var n uint32
	if windows.DeviceIoControl(h, ioctlDiskGetPartitionInfoEx, nil, 0, (*byte)(unsafe.Pointer(&part)), uint32(unsafe.Sizeof(part)), &n, nil) != nil {
		return fallback
	}
	switch strings.ToUpper(fsName) {
	case "EXFAT":
		return fsUUID(serial)
	case "FAT", "FAT32":
		var spc, bps, free, clusters uint32
		root, _ := windows.UTF16PtrFromString(letter + `\`)
		if r, _, _ := getDiskFreeSpace.Call(uintptr(unsafe.Pointer(root)), uintptr(unsafe.Pointer(&spc)), uintptr(unsafe.Pointer(&bps)),
			uintptr(unsafe.Pointer(&free)), uintptr(unsafe.Pointer(&clusters))); r == 0 || bps == 0 {
			return fallback
		}
		return fsUUID(serial, uint32(part.Length/int64(bps))) // the boot sector's total sectors: the whole partition
	case "NTFS":
		if part.Style == 1 {
			return strings.Trim(part.GptID.String(), "{}")
		}
	}
	return fallback
}

// lowPriority: the idle priority class, Windows' nearest to macOS's background priority.
func lowPriority(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.IDLE_PRIORITY_CLASS}
}

func revealFile(path string) error { return exec.Command("explorer", "/select,", path).Run() }

func openBrowser(url string) error {
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

var getSystemPowerStatus = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetSystemPowerStatus")

// watchPower: on battery and battery saver, from GetSystemPowerStatus every 30 seconds. Windows has no
// thermal state to read here, and the metered connection needs WinRT: neither pauses the work on Windows.
func (a *App) watchPower() {
	for {
		var s struct {
			ACLine, BatteryFlag, Percent, SaverOn uint8 // SYSTEM_POWER_STATUS
			LifeTime, FullLifeTime                uint32
		}
		if r, _, _ := getSystemPowerStatus.Call(uintptr(unsafe.Pointer(&s))); r != 0 {
			a.smu.Lock()
			a.cond = conditions{Battery: s.ACLine == 0, LowPower: s.SaverOn == 1}
			a.smu.Unlock()
		}
		time.Sleep(30 * time.Second)
	}
}
