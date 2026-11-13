package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestWindowsDrives: the system drive is identified by its serial number and never offered as a drive.
func TestWindowsDrives(t *testing.T) {
	sys := os.Getenv("SystemDrive") + `\`
	if slices.Contains(driveRoots(), sys) {
		t.Fatalf("the system drive %s is listed among the drives", sys)
	}
	d, err := identifyWindows(sys)
	if err != nil || len(d.UUID) != 36 || d.Size <= 0 || d.FS != "NTFS" { // NTFS on GPT: the partition's GUID
		t.Fatalf("identify %s: %+v %v", sys, d, err)
	}
	t.Logf("video codec: %v", videoCodec())
}

// TestDriveIdentity makes real virtual disks (diskpart: needs an administrator, so only in CI) and checks
// that the identity Windows computes is the Mac's: from the boot sector read raw, the serial number (and for
// FAT the total sectors) that fsUUID turns into the Mac's UUID; for NTFS on GPT the partition's GUID.
func TestDriveIdentity(t *testing.T) {
	tmp := os.Getenv("RUNNER_TEMP") // a long path: diskpart does not match a vdisk by its 8.3 short name (RUNNER~1)
	if tmp == "" {
		t.Skip("creates virtual disks: CI only")
	}
	for _, c := range []struct{ style, fs string }{{"mbr", "exfat"}, {"gpt", "exfat"}, {"mbr", "fat32"}, {"gpt", "ntfs"}, {"mbr", "ntfs"}} {
		t.Run(c.style+"-"+c.fs, func(t *testing.T) {
			vhd := filepath.Join(tmp, c.style+"-"+c.fs+".vhd")
			diskpart(t, fmt.Sprintf("create vdisk file=\"%s\" maximum=300 type=fixed\nselect vdisk file=\"%s\"\nattach vdisk\nconvert %s\n"+
				"create partition primary\nformat fs=%s quick label=RB\nassign letter=R\n", vhd, vhd, c.style, c.fs))
			defer diskpart(t, fmt.Sprintf("select vdisk file=\"%s\"\ndetach vdisk\n", vhd))
			d, err := identifyWindows(`R:\`)
			if err != nil {
				t.Fatal(err)
			}
			boot := make([]byte, 512)
			f, err := os.Open(`\\.\R:`)
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.Read(boot)
			f.Close()
			if err != nil {
				t.Fatal(err)
			}
			var want string
			switch c.fs {
			case "exfat":
				want = fsUUID(binary.LittleEndian.Uint32(boot[0x64:]))
			case "fat32":
				sectors := uint32(binary.LittleEndian.Uint16(boot[0x13:]))
				if sectors == 0 {
					sectors = binary.LittleEndian.Uint32(boot[0x20:])
				}
				want = fsUUID(binary.LittleEndian.Uint32(boot[0x43:]), sectors)
			case "ntfs":
				want = fmt.Sprintf("WIN-%08X", binary.LittleEndian.Uint32(boot[0x48:])) // the low half of NTFS's serial
				if c.style == "gpt" {
					out, err := exec.Command("powershell", "-NoProfile", "-Command", "(Get-Partition -DriveLetter R).Guid").Output()
					if err != nil {
						t.Fatal(err)
					}
					want = strings.ToUpper(strings.Trim(strings.TrimSpace(string(out)), "{}"))
				}
			}
			if d.UUID != want {
				t.Fatalf("%s %s: Windows says %s, the boot sector gives %s", c.style, c.fs, d.UUID, want)
			}
			t.Logf("%s %s: %s (%s)", c.style, c.fs, d.UUID, d.FS)
		})
	}
}

func diskpart(t *testing.T, script string) {
	p := filepath.Join(t.TempDir(), "dp.txt")
	os.WriteFile(p, []byte(script), 0o644)
	if out, err := exec.Command("diskpart", "/s", p).CombinedOutput(); err != nil {
		t.Fatalf("diskpart: %v\n%s", err, out)
	}
}
