package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMade: provenance read from the tags real files carry (from the drives of Artide's tests), the camera's
// sidecar, and deduced only from names and places, marked with ?.
func TestMade(t *testing.T) {
	dir := t.TempDir()
	mxf := filepath.Join(dir, "576_0090.MXF")
	must(t, os.WriteFile(mxf, nil, 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "576_0090M01.XML"), []byte(`<Device manufacturer="Sony" modelName="ILME-FX6V" serialNo="1"/>`), 0o644))
	for _, c := range []struct {
		p    string
		tags map[string]string
		want string
	}{
		{"/v/Nayt v2.mp4", map[string]string{"encoder": "Blackmagic Design DaVinci Resolve Studio", "s.encoder": "H.264"}, "export:DaVinci Resolve"},
		{"/v/IMG_0001.MOV", map[string]string{"com.apple.quicktime.make": "Apple", "com.apple.quicktime.model": "iPhone 16 Pro Max"}, "camera:Apple iPhone 16 Pro Max"},
		{"/v/Audio/Arisa.WAV", map[string]string{"encoded_by": "SoundDev: MixPre-10 II TJ0120211"}, "recorder:SoundDev MixPre-10 II"},
		{"/v/A001.RDC/A001.wav", map[string]string{"encoded_by": "Red Digital Cinema, LLC"}, "camera:RED"},
		{mxf, map[string]string{"company_name": "Sony"}, "camera:Sony ILME-FX6V"},
		{"/v/428_0031.MXF", map[string]string{"company_name": "Sony", "product_name": "Mem"}, "camera:Sony"},
		{"/v/Drone 3/DJI_0495.MOV", map[string]string{"encoder": "Lavf56.15.102"}, "camera?:DJI"},
		{"/v/clip.mp4", map[string]string{"encoder": "Lavf60.3.100"}, ""},
		{"/v/PROJ/Adobe Premiere Pro Video Previews/SSS.PRV/Rendered - 1.mov", nil, "preview?:Adobe Premiere Pro"},
		{"/v/BUDDY FAST EXPORT/XPORT/NR.mov", nil, "export?:"},
		{"/v/GH010001.MP4", map[string]string{"s.handler_name": "GoPro AVC"}, "camera:GoPro"},
	} {
		if got := made(c.p, c.tags); got != c.want {
			t.Errorf("%s: %q, want %q", filepath.Base(c.p), got, c.want)
		}
	}
}

// TestMadeCatalog: a drive counts its clips by what made them, and the archive filters originals and exports.
func TestMadeCatalog(t *testing.T) {
	recs := `{"Path":"/v/A/576_0001.MXF","Ext":".mxf","Hash":"a","Size":1,"Mtime":"2026-01-01T00:00:00Z","Dur":5,"Codec":"h264","Made":"camera:Sony ILME-FX6V"}
{"Path":"/v/A/DJI_0001.MOV","Ext":".mov","Hash":"b","Size":1,"Mtime":"2026-01-01T00:00:00Z","Dur":5,"Codec":"h264","Made":"camera?:DJI"}
{"Path":"/v/A/Nayt v2.mp4","Ext":".mp4","Hash":"c","Size":1,"Mtime":"2026-01-01T00:00:00Z","Dur":5,"Codec":"h264","Made":"export:DaVinci Resolve"}
{"Path":"/v/A/x.mp4","Ext":".mp4","Hash":"d","Size":1,"Mtime":"2026-01-01T00:00:00Z","Dur":5,"Codec":"h264"}
`
	c, err := buildCatalog([]drive{{Device: Device{UUID: "A", Name: "A"}, files: strings.NewReader(recs)}}, nil, nil, nil)
	must(t, err)
	if m := c.Overview().Devices[0].Made; m["camera:Sony ILME-FX6V"] != 1 || m["camera:DJI"] != 1 || m["export:DaVinci Resolve"] != 1 || len(m) != 3 {
		t.Fatalf("drive made %v", m)
	}
	names := func(flag string) (s []string) {
		for _, r := range c.Clips(Query{Flags: []string{flag}, All: true}) {
			s = append(s, r.Name)
		}
		return s
	}
	if o, e := names("original"), names("export"); len(o) != 2 || len(e) != 1 || e[0] != "Nayt v2.mp4" {
		t.Fatalf("originals %v, exports %v", o, e)
	}
}
