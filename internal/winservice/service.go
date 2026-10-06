//go:build windows

// Package winservice mengintegrasikan diskhist.exe dengan Windows Service
// Control Manager (SCM) menggunakan golang.org/x/sys/windows/svc.
package winservice

import (
	"context"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const ServiceName = "DiskHistMonitor"

// serviceHandler mengimplementasikan svc.Handler untuk Windows Service.
type serviceHandler struct {
	cancel context.CancelFunc
	done   chan struct{}
	runFn  func(context.Context)
}

func (h *serviceHandler) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (ssec bool, errno uint32) {
	changes <- svc.Status{State: svc.StartPending}
	runCtx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan struct{})
	go func() {
		defer close(h.done)
		h.runFn(runCtx)
	}()
	changes <- svc.Status{
		Accepts: svc.AcceptStop | svc.AcceptShutdown,
		State:   svc.Running,
	}

	for {
		select {
		case req, ok := <-r:
			if !ok {
				cancel()
				<-h.done
				return false, 0
			}
			switch req.Cmd {
			case svc.Stop, svc.Shutdown:
				goto stopping
			case svc.Pause:
				changes <- svc.Status{State: svc.Paused, Accepts: svc.AcceptStop | svc.AcceptShutdown}
			case svc.Continue:
				changes <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
			}
		}
	}

stopping:
	// Batalkan scan aktif dan TUNGGU sampai cleanup VSS selesai, dengan batas
	// waktu. Ini mencegah snapshot VSS ditinggalkan saat service di-stop.
	changes <- svc.Status{State: svc.StopPending}
	cancel()
	select {
	case <-h.done:
	case <-time.After(2 * time.Minute):
	}
	changes <- svc.Status{State: svc.Stopped}
	return false, 0
}

// RunAsService dipanggil dari main.go saat SCM memulai service.
func RunAsService(runFn func(context.Context)) error {
	handler := &serviceHandler{runFn: runFn}
	return svc.Run(ServiceName, handler)
}

// InstallService mendaftarkan diskhist.exe sebagai Windows Service.
func InstallService() error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("gagal mendapatkan path executable: %w", err)
	}

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("gagal terhubung ke Service Manager: %w (pastikan dijalankan sebagai Administrator)", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(ServiceName)
	if err == nil {
		s.Close()
		return fmt.Errorf("service %q sudah terinstall — uninstall dulu sebelum install ulang", ServiceName)
	}

	s, err = m.CreateService(ServiceName, exePath, mgr.Config{
		DisplayName:  "DiskHist Monitor",
		Description:  "Automated disk usage history scanner with web dashboard",
		StartType:    mgr.StartAutomatic,
		ErrorControl: mgr.ErrorNormal,
	})
	if err != nil {
		return fmt.Errorf("gagal membuat service %q: %w", ServiceName, err)
	}
	defer s.Close()

	// NFR-06: Set recovery actions — restart setelah 1m, 5m, 10m
	recoveryActions := []mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 1 * time.Minute},
		{Type: mgr.ServiceRestart, Delay: 5 * time.Minute},
		{Type: mgr.ServiceRestart, Delay: 10 * time.Minute},
	}
	if err := s.SetRecoveryActions(recoveryActions, 86400); err != nil {
		fmt.Printf("[WARN] gagal set recovery actions via API, coba sc.exe manual: %v\n", err)
	}

	fmt.Printf("[OK] service %q berhasil diinstall (auto-start, recovery restart aktif)\n", ServiceName)
	fmt.Printf("     Path: %s\n", exePath)
	fmt.Printf("     Jalankan: sc.exe start %s\n", ServiceName)
	return nil
}

// UninstallService menghentikan dan menghapus Windows Service.
func UninstallService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("gagal terhubung ke Service Manager: %w (pastikan dijalankan sebagai Administrator)", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(ServiceName)
	if err != nil {
		return fmt.Errorf("service %q tidak ditemukan: %w", ServiceName, err)
	}
	defer s.Close()

	status, err := s.Query()
	if err == nil && status.State != svc.Stopped {
		fmt.Printf("[INFO] menghentikan service %q...\n", ServiceName)
		_, err = s.Control(svc.Stop)
		if err != nil {
			fmt.Printf("[WARN] gagal menghentikan service: %v\n", err)
		}
		time.Sleep(3 * time.Second)
	}

	if err := s.Delete(); err != nil {
		return fmt.Errorf("gagal menghapus service %q: %w", ServiceName, err)
	}

	fmt.Printf("[OK] service %q berhasil diuninstall\n", ServiceName)
	return nil
}

// IsServiceMode mendeteksi apakah proses dijalankan oleh SCM.
func IsServiceMode() bool {
	interactive, _ := svc.IsAnInteractiveSession()
	return !interactive
}
