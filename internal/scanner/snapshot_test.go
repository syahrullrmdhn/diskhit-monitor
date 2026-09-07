package scanner

import (
	"strings"
	"testing"
)

func TestValidateWindowsVolumeRoot(t *testing.T) {
	for _, root := range []string{`C:\`, `z:\`} {
		if err := validateWindowsVolumeRoot(root); err != nil {
			t.Errorf("validateWindowsVolumeRoot(%q): %v", root, err)
		}
	}
	for _, root := range []string{`C:`, `C:\data`, `\\server\share\`, `C:\'; Remove-Item C:\ -Recurse; '`} {
		if err := validateWindowsVolumeRoot(root); err == nil {
			t.Errorf("validateWindowsVolumeRoot(%q) succeeded, want error", root)
		}
	}
}

func TestParseShadowCopyJSONMapsDeviceToSourceRoot(t *testing.T) {
	raw := []byte("DISKHIST_SHADOW_ID={abc-123}\r\n" + `{"ID":"{abc-123}","DeviceObject":"\\\\?\\GLOBALROOT\\Device\\HarddiskVolumeShadowCopy7"}`)
	shadow, err := parseShadowCopyJSON(raw, `D:\`)
	if err != nil {
		t.Fatal(err)
	}
	if shadow.ID != "{abc-123}" {
		t.Fatalf("ID=%q", shadow.ID)
	}
	if shadow.SourceRoot != `\\?\GLOBALROOT\Device\HarddiskVolumeShadowCopy7\` {
		t.Fatalf("SourceRoot=%q", shadow.SourceRoot)
	}
	if shadow.ReportedRoot != `D:\` {
		t.Fatalf("ReportedRoot=%q", shadow.ReportedRoot)
	}
	if id := extractShadowID(raw); id != "{abc-123}" {
		t.Fatalf("extractShadowID=%q", id)
	}
}

func TestParseShadowCopyJSONRejectsUnexpectedDevice(t *testing.T) {
	_, err := parseShadowCopyJSON([]byte(`{"ID":"x","DeviceObject":"C:\\temp"}`), `C:\`)
	if err == nil || !strings.Contains(err.Error(), "DeviceObject") {
		t.Fatalf("err=%v, want DeviceObject validation error", err)
	}
}

func TestPowerShellScriptsDoNotInterpolateInputs(t *testing.T) {
	for _, script := range []string{createShadowScript, deleteShadowScript} {
		if strings.Contains(script, "%s") || strings.Contains(script, "+ $env:") {
			t.Fatalf("script appears to interpolate input: %s", script)
		}
	}
	if !strings.Contains(createShadowScript, "$env:DISKHIST_VOLUME") {
		t.Fatal("create script must read validated volume from environment")
	}
	if !strings.Contains(deleteShadowScript, "$env:DISKHIST_SHADOW_ID") {
		t.Fatal("delete script must read shadow ID from environment")
	}
}
