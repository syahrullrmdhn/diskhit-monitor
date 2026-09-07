//go:build windows

package scanner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

type vssProvider struct {
	commandTimeout time.Duration
}

func NewSnapshotProvider(commandTimeout time.Duration) SnapshotProvider {
	return &vssProvider{commandTimeout: commandTimeout}
}

func (p *vssProvider) Create(ctx context.Context, volume string) (Snapshot, error) {
	if err := validateWindowsVolumeRoot(volume); err != nil {
		return Snapshot{}, err
	}
	output, err := p.run(ctx, "DISKHIST_VOLUME", volume, createShadowScript)
	if err != nil {
		p.cleanupID(extractShadowID(output))
		return Snapshot{}, fmt.Errorf("create VSS snapshot for %q: %w", volume, err)
	}
	shadow, err := parseShadowCopyJSON(output, volume)
	if err != nil {
		p.cleanupID(extractShadowID(output))
		return Snapshot{}, err
	}
	return shadow, nil
}

func (p *vssProvider) cleanupID(id string) {
	if id == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), p.commandTimeout)
	defer cancel()
	if err := p.Delete(ctx, Snapshot{ID: id}); err != nil {
		// Best effort here; caller will report the create failure.
		_ = err
	}
}

func (p *vssProvider) Delete(ctx context.Context, shadow Snapshot) error {
	if strings.TrimSpace(shadow.ID) == "" || strings.ContainsAny(shadow.ID, "\r\n\x00") {
		return fmt.Errorf("invalid empty or multiline shadow copy ID")
	}
	_, err := p.run(ctx, "DISKHIST_SHADOW_ID", shadow.ID, deleteShadowScript)
	if err != nil {
		return fmt.Errorf("delete VSS snapshot %q: %w", shadow.ID, err)
	}
	return nil
}

func (p *vssProvider) run(parent context.Context, envName, envValue, script string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, p.commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	cmd.Env = append(os.Environ(), envName+"="+envValue)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return output, fmt.Errorf("PowerShell timed out after %s: %w", p.commandTimeout, ctx.Err())
	}
	if err != nil {
		return output, fmt.Errorf("PowerShell failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return output, nil
}
