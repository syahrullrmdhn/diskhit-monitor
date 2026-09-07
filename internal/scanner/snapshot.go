package scanner

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Snapshot identifies a scan source and the configured path used for reports.
type Snapshot struct {
	ID           string
	SourceRoot   string
	ReportedRoot string
	Consistent   bool
}

// SnapshotProvider creates short-lived point-in-time scan sources.
type SnapshotProvider interface {
	Create(context.Context, string) (Snapshot, error)
	Delete(context.Context, Snapshot) error
}

var windowsVolumeRoot = regexp.MustCompile(`^[A-Za-z]:\\$`)

func validateWindowsVolumeRoot(root string) error {
	if !windowsVolumeRoot.MatchString(root) {
		return fmt.Errorf("drive %q must be an absolute Windows volume root such as D:\\", root)
	}
	return nil
}

func extractShadowID(raw []byte) string {
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "DISKHIST_SHADOW_ID=") {
			return strings.TrimSpace(strings.TrimPrefix(line, "DISKHIST_SHADOW_ID="))
		}
	}
	return ""
}

func parseShadowCopyJSON(raw []byte, reportedRoot string) (Snapshot, error) {
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	jsonLine := strings.TrimSpace(lines[len(lines)-1])
	var value struct {
		ID           string `json:"ID"`
		DeviceObject string `json:"DeviceObject"`
	}
	if err := json.Unmarshal([]byte(jsonLine), &value); err != nil {
		return Snapshot{}, fmt.Errorf("invalid Win32_ShadowCopy JSON: %w", err)
	}
	if strings.TrimSpace(value.ID) == "" {
		return Snapshot{}, fmt.Errorf("Win32_ShadowCopy JSON has empty ID")
	}
	device := strings.TrimRight(value.DeviceObject, `\`)
	if !strings.HasPrefix(strings.ToLower(device), `\\?\globalroot\device\harddiskvolumeshadowcopy`) {
		return Snapshot{}, fmt.Errorf("unexpected Win32_ShadowCopy DeviceObject %q", value.DeviceObject)
	}
	return Snapshot{ID: value.ID, SourceRoot: device + `\`, ReportedRoot: reportedRoot, Consistent: true}, nil
}

const createShadowScript = `$ErrorActionPreference='Stop'
$r=Invoke-CimMethod -ClassName Win32_ShadowCopy -MethodName Create -Arguments @{Volume=$env:DISKHIST_VOLUME;Context='ClientAccessible'}
if ($r.ReturnValue -ne 0) { throw "Win32_ShadowCopy.Create returned $($r.ReturnValue)" }
Write-Output ("DISKHIST_SHADOW_ID=" + $r.ShadowID)
try {
  $s=Get-CimInstance -ClassName Win32_ShadowCopy | Where-Object { $_.ID -eq $r.ShadowID } | Select-Object -First 1 ID,DeviceObject
  if ($null -eq $s) { throw 'created shadow copy was not found' }
  $s | ConvertTo-Json -Compress
} catch {
  Get-CimInstance -ClassName Win32_ShadowCopy | Where-Object { $_.ID -eq $r.ShadowID } | Remove-CimInstance -ErrorAction SilentlyContinue
  throw
}`

const deleteShadowScript = `$ErrorActionPreference='Stop'
$s=Get-CimInstance -ClassName Win32_ShadowCopy | Where-Object { $_.ID -eq $env:DISKHIST_SHADOW_ID } | Select-Object -First 1
if ($null -ne $s) { $s | Remove-CimInstance }`
