package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Provenance: what made a file, read from the file itself (its tags, its sidecar) when it says so, so that
// rushbin can tell the camera originals from the exports and the sound recordings. A record's Made is
// "kind:maker", kind one of camera, recorder, export, preview. A kind ending in ? is deduced from the file's
// name or place, not read: rushbin says so wherever it shows it. Empty: the file says nothing.

// probeVersion: records probed before it have no provenance, so they are probed once more.
// 3: the day of the shoot from RED's metadata and from MXF headers.
const probeVersion = 3

// editors are the apps whose name in a file's encoder tag makes it an export.
var editors = []struct{ match, name string }{
	{"DaVinci Resolve", "DaVinci Resolve"}, {"Premiere", "Adobe Premiere Pro"}, {"Media Encoder", "Adobe Media Encoder"},
	{"After Effects", "Adobe After Effects"}, {"Final Cut", "Final Cut Pro"}, {"Compressor", "Apple Compressor"},
	{"Avid", "Avid Media Composer"}, {"Pro Tools", "Pro Tools"}, {"HandBrake", "HandBrake"}, {"CapCut", "CapCut"},
	{"clipchamp", "Clipchamp"}, {"Resolume", "Resolume"}, {"FL Studio", "FL Studio"}, {"Logic Pro", "Logic Pro"},
}

var (
	serial    = regexp.MustCompile(`\s+[A-Z]*\d{4,}\w*$`) // a recorder's serial number after its model
	sonyModel = regexp.MustCompile(`<Device[^>]*manufacturer="([^"]+)"[^>]*modelName="([^"]+)"`)
	djiName   = regexp.MustCompile(`(?i)^dji_\d{4}`)
	outFolder = regexp.MustCompile(`(?i)^(export|exports|xport|render|renders|output|deliver|deliverables|consegne)$`)
)

// made reads the provenance of the file at p from the tags ffprobe gave (format's and every stream's, keys in
// lower case).
func made(p string, tags map[string]string) string {
	editor := func(v string) string {
		for _, e := range editors {
			if strings.Contains(strings.ToLower(v), strings.ToLower(e.match)) {
				return e.name
			}
		}
		return ""
	}
	if m := tags["com.apple.quicktime.model"]; m != "" { // phones and many cameras
		return "camera:" + strings.TrimSpace(tags["com.apple.quicktime.make"]+" "+m)
	}
	if m := tags["model"]; m != "" {
		return "camera:" + strings.TrimSpace(tags["make"]+" "+m)
	}
	for _, k := range []string{"encoder", "s.encoder", "encoded_by", "originator", "software", "com.apple.quicktime.software"} {
		if e := editor(tags[k]); e != "" {
			return "export:" + e
		}
	}
	if v := tags["encoded_by"] + tags["originator"]; v != "" { // a sound recorder writes itself in the Broadcast Wave
		v = strings.TrimSpace(serial.ReplaceAllString(strings.ReplaceAll(v, ":", ""), ""))
		if strings.Contains(strings.ToLower(v), "red digital cinema") {
			return "camera:RED"
		}
		return "recorder:" + v
	}
	if h := tags["s.handler_name"] + " " + tags["encoder"]; strings.Contains(h, "GoPro") {
		return "camera:GoPro"
	} else if strings.Contains(h, "DJI") {
		return "camera:DJI"
	}
	if strings.EqualFold(filepath.Ext(p), ".mxf") {
		if b, err := os.ReadFile(strings.TrimSuffix(p, filepath.Ext(p)) + "M01.XML"); err == nil { // the camera's sidecar
			if m := sonyModel.FindSubmatch(b); m != nil {
				return "camera:" + string(m[1]) + " " + string(m[2])
			}
		}
		if c := tags["company_name"]; c != "" {
			return "camera:" + c
		}
	}
	// Deduced, not read: DJI's older drones write only the muxer (Lavf, also any ffmpeg conversion) and their names.
	if djiName.MatchString(filepath.Base(p)) {
		return "camera?:DJI"
	}
	for _, part := range strings.Split(filepath.ToSlash(filepath.Dir(p)), "/") {
		if strings.Contains(part, "Video Previews") || strings.HasSuffix(part, ".PRV") {
			return "preview?:Adobe Premiere Pro"
		}
		if outFolder.MatchString(part) {
			return "export?:"
		}
	}
	return ""
}

// madeKind is the kind of a provenance, read or deduced alike: camera, recorder, export, preview, or "".
func madeKind(m string) string {
	k, _, _ := strings.Cut(m, ":")
	return strings.TrimSuffix(k, "?")
}
