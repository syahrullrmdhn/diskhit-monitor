//go:build !windows

// Stub untuk build di non-Windows (Linux/Mac) — agar project tetap bisa
// di-compile dan di-test logic inti (scanner, config, storage, webserver)
// tanpa harus di lingkungan Windows.
package winservice

import "fmt"

const ServiceName = "DiskHistMonitor"

func RunAsService(runFn func()) error {
	return fmt.Errorf("RunAsService hanya tersedia di Windows")
}

func InstallService() error {
	return fmt.Errorf("install service hanya bisa dijalankan di Windows — build dengan GOOS=windows")
}

func UninstallService() error {
	return fmt.Errorf("uninstall service hanya bisa dijalankan di Windows — build dengan GOOS=windows")
}

func IsServiceMode() bool {
	return false
}
