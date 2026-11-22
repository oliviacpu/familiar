//go:build windows

package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"strings"

	"golang.org/x/sys/windows"
)

// Reading each drive's own health data needs administrator rights, which
// familiar has from its scheduled task.

const (
	ioctlStorageQueryProperty = 0x002D1400
	ioctlSmartRcvDriveData    = 0x0007C088

	storageDeviceProperty                 = 0
	storageDeviceProtocolSpecificProperty = 50
	protocolTypeNvme                      = 3
	nvmeDataTypeLogPage                   = 2
	nvmeLogPageHealthInfo                 = 2

	busATA, busUSB, busRAID, busSATA, busSD, busMMC, busNVMe = 3, 7, 8, 11, 12, 13, 17
)

// removableBus: plugged-in drives, left out of storage and health.
func removableBus(bus uint32, removableMedia bool) bool {
	return removableMedia || bus == busUSB || bus == busSD || bus == busMMC
}

// removableVolume is whether the drive behind a letter ("D:") is plugged in
// rather than built in. Unknown counts as built in.
func removableVolume(letter string) bool {
	path, _ := windows.UTF16PtrFromString(`\\.\` + letter)
	h, err := windows.CreateFile(path, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	desc, err := ioctl(h, ioctlStorageQueryProperty, make([]byte, 12), 1024)
	if err != nil || len(desc) < 32 {
		return false
	}
	return removableBus(binary.LittleEndian.Uint32(desc[28:32]), desc[10] != 0)
}

func busName(t uint32) string {
	switch t {
	case busNVMe:
		return "NVMe"
	case busSATA, busATA:
		return "SATA"
	case busUSB:
		return "USB"
	case busRAID:
		return "RAID"
	}
	return fmt.Sprintf("bus %d", t)
}

func readDriveHealth(ctx context.Context) []DriveHealth {
	var ds []DriveHealth
	for n := 0; n < 32 && ctx.Err() == nil; n++ {
		if d, ok := driveHealth(n); ok {
			ds = append(ds, d)
		}
	}
	return ds
}

// driveHealth reads \\.\PhysicalDriveN: Disk N in Disk Management.
func driveHealth(n int) (DriveHealth, bool) {
	path, _ := windows.UTF16PtrFromString(fmt.Sprintf(`\\.\PhysicalDrive%d`, n))
	h, err := windows.CreateFile(path, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return DriveHealth{}, false // no such drive
	}
	defer windows.CloseHandle(h)
	d := DriveHealth{Device: fmt.Sprintf("Disk %d", n), Status: "Not reported"}
	bus := uint32(0)
	if desc, err := ioctl(h, ioctlStorageQueryProperty, make([]byte, 12), 1024); err == nil && len(desc) >= 32 {
		bus = binary.LittleEndian.Uint32(desc[28:32])
		if removableBus(bus, desc[10] != 0) {
			return DriveHealth{}, false // plugged in, not part of this PC
		}
		d.Bus = busName(bus)
		d.Model = descString(desc, binary.LittleEndian.Uint32(desc[16:20]))
		if v := descString(desc, binary.LittleEndian.Uint32(desc[12:16])); v != "" && !strings.HasPrefix(strings.ToUpper(d.Model), strings.ToUpper(v)) && !strings.EqualFold(v, "NVMe") {
			d.Model = v + " " + d.Model
		}
	}
	switch bus {
	case busNVMe:
		if log, err := nvmeHealthLog(h); err == nil {
			readNVMeLog(&d, log)
		}
	case busSATA, busATA:
		data, err1 := smartRead(h, n, 0xD0)
		thresholds, _ := smartRead(h, n, 0xD1)
		if err1 == nil {
			readATASmart(&d, data, thresholds)
		}
	}
	return d, true
}

// nvmeHealthLog asks the NVMe driver for log page 2, Microsoft's documented
// way ("Working with NVMe drives": StorageDeviceProtocolSpecificProperty).
func nvmeHealthLog(h windows.Handle) ([]byte, error) {
	const header = 8 // STORAGE_PROPERTY_QUERY up to AdditionalParameters
	const proto = 40 // STORAGE_PROTOCOL_SPECIFIC_DATA
	const logSize = 512
	in := make([]byte, header+proto+logSize)
	le := binary.LittleEndian
	le.PutUint32(in[0:], storageDeviceProtocolSpecificProperty)
	le.PutUint32(in[4:], 0) // PropertyStandardQuery
	p := in[header:]
	le.PutUint32(p[0:], protocolTypeNvme)
	le.PutUint32(p[4:], nvmeDataTypeLogPage)
	le.PutUint32(p[8:], nvmeLogPageHealthInfo)
	le.PutUint32(p[16:], proto)   // ProtocolDataOffset, from the start of this struct
	le.PutUint32(p[20:], logSize) // ProtocolDataLength
	out, err := ioctl(h, ioctlStorageQueryProperty, in, len(in))
	if err != nil {
		return nil, err
	}
	// STORAGE_PROTOCOL_DATA_DESCRIPTOR: Version, Size, then the protocol data struct.
	if len(out) < 8+proto {
		return nil, fmt.Errorf("short answer (%d bytes)", len(out))
	}
	off := 8 + int(le.Uint32(out[8+16:]))
	length := int(le.Uint32(out[8+20:]))
	if length > logSize {
		length = logSize
	}
	if off+length > len(out) || length < 192 {
		return nil, fmt.Errorf("log out of range (offset %d, length %d, have %d)", off, length, len(out))
	}
	return out[off : off+length], nil
}

// smartRead sends SMART READ DATA (0xD0) or READ THRESHOLDS (0xD1) through
// SMART_RCV_DRIVE_DATA. SENDCMDINPARAMS is 32 bytes packed; the answer,
// SENDCMDOUTPARAMS, has the 512 bytes after a 16-byte header.
func smartRead(h windows.Handle, drive int, feature byte) ([]byte, error) {
	in := make([]byte, 32)
	binary.LittleEndian.PutUint32(in[0:], 512) // cBufferSize
	regs := in[4:12]                           // IDEREGS
	regs[0], regs[1], regs[2] = feature, 1, 1
	regs[3], regs[4] = 0x4F, 0xC2 // SMART signature
	regs[5] = 0xA0 | byte(drive&1)<<4
	regs[6] = 0xB0 // SMART command
	in[12] = byte(drive)
	out, err := ioctl(h, ioctlSmartRcvDriveData, in, 16+512)
	if err != nil {
		return nil, err
	}
	if len(out) < 16+362 {
		return nil, fmt.Errorf("short SMART answer (%d bytes)", len(out))
	}
	return out[16:], nil
}

func ioctl(h windows.Handle, code uint32, in []byte, outSize int) ([]byte, error) {
	out := make([]byte, outSize)
	var got uint32
	if err := windows.DeviceIoControl(h, code, &in[0], uint32(len(in)), &out[0], uint32(len(out)), &got, nil); err != nil {
		return nil, err
	}
	return out[:got], nil
}

// descString reads a NUL-terminated string at off in a STORAGE_DEVICE_DESCRIPTOR.
func descString(b []byte, off uint32) string {
	if off == 0 || int(off) >= len(b) {
		return ""
	}
	s := b[off:]
	if i := strings.IndexByte(string(s), 0); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(string(s))
}

func collectDriveHealth(ctx context.Context) []DriveHealth { return readDriveHealth(ctx) }
