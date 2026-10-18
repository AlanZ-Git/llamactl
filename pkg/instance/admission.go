package instance

import (
	"errors"
	"fmt"
	"llamactl/pkg/resourcepolicy"
	"net/http"
	"sync"
)

// AdmissionError is a stable control/proxy rejection, including wrapped errors.
type AdmissionError struct {
	Code    string
	Details string
}

func (e *AdmissionError) Error() string { return e.Code + ": " + e.Details }
func BusyError() error {
	return &AdmissionError{Code: "busy", Details: "instance has active use or maintenance"}
}

// AdmissionHTTPError maps admission failures for both native control and proxies.
func AdmissionHTTPError(err error) (int, string, bool) {
	if errors.Is(err, resourcepolicy.ErrPaused) {
		return http.StatusConflict, "ai_paused", true
	}
	var admission *AdmissionError
	if !errors.As(err, &admission) {
		return 0, "", false
	}
	status := http.StatusConflict
	if admission.Code == "instance_not_running" {
		status = http.StatusServiceUnavailable
	}
	if admission.Code == "unsupported_controlled_request" {
		status = http.StatusBadRequest
	}
	return status, admission.Code, true
}

type admissionGate struct {
	mu          sync.Mutex
	uses        int
	maintenance map[*Maintenance]struct{}
	retired     bool
	transition  bool
}

// Maintenance is a checked, instance-bound capability. It holds no gate mutex
// during IO. Close invalidates every operation and prepared candidate using it.
// The private operation mutex prevents concurrent reuse or close of a capability.
type Maintenance struct {
	mu       sync.Mutex
	instance *Instance
	closed   bool
	permit   *resourcepolicy.Permit
}

// BeginWorkMaintenance checks the owner latch before ordinary instance contention.
func (i *Instance) BeginWorkMaintenance() (*Maintenance, error) { return i.beginMaintenance(true) }

func (m *Maintenance) checkPolicy() error {
	if m.instance.policy != nil && m.permit == nil {
		return resourcepolicy.ErrPaused
	}
	return m.permit.Check()
}

func (i *Instance) IsControlled() bool {
	opts := i.GetOptions()
	return opts != nil && opts.Controlled
}

// BeginMaintenance rejects controlled contention immediately rather than queueing.
// Legacy operations retain their original manager/process serialization. Their
// leases and uses are tracked so enabling controlled cannot race existing work.
func (i *Instance) BeginMaintenance() (*Maintenance, error) { return i.beginMaintenance(false) }

func (i *Instance) beginMaintenance(work bool) (*Maintenance, error) {
	permit, err := i.policy.Acquire()
	if err != nil && work {
		return nil, err
	}
	accepted := false
	defer func() {
		if !accepted {
			permit.Close()
		}
	}()
	i.admission.mu.Lock()
	defer i.admission.mu.Unlock()
	if i.admission.retired || i.admission.transition ||
		(i.IsControlled() && (len(i.admission.maintenance) != 0 || i.admission.uses != 0)) {
		return nil, BusyError()
	}
	m := &Maintenance{instance: i, permit: permit}
	accepted = true
	if i.admission.maintenance == nil {
		i.admission.maintenance = make(map[*Maintenance]struct{})
	}
	i.admission.maintenance[m] = struct{}{}
	return m, nil
}

func (m *Maintenance) valid() error {
	if m == nil || m.instance == nil || m.closed {
		return fmt.Errorf("maintenance lease is closed or invalid")
	}
	m.instance.admission.mu.Lock()
	_, active := m.instance.admission.maintenance[m]
	retired := m.instance.admission.retired
	m.instance.admission.mu.Unlock()
	if !active || retired {
		return fmt.Errorf("maintenance lease is not active")
	}
	return nil
}

func (m *Maintenance) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.instance == nil || m.closed {
		return
	}
	i := m.instance
	defer m.permit.Close()
	i.admission.mu.Lock()
	defer i.admission.mu.Unlock()
	if _, active := i.admission.maintenance[m]; !active {
		return
	}
	delete(i.admission.maintenance, m)
	// A transition requires this to be the sole maintenance lease.
	i.admission.transition = false
	m.closed = true
}

// CheckModeChange runs before reserving ports or preparing a preset.
func (m *Maintenance) CheckModeChange(opts *Options) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.valid(); err != nil {
		return err
	}
	return m.checkModeChange(opts)
}

func (m *Maintenance) checkModeChange(opts *Options) error {
	if err := m.checkPolicy(); err != nil {
		return err
	}
	if opts == nil {
		return fmt.Errorf("instance options cannot be nil")
	}
	i := m.instance
	i.admission.mu.Lock()
	defer i.admission.mu.Unlock()
	if opts.Controlled != i.IsControlled() {
		if i.GetStatus() != Stopped {
			return &AdmissionError{Code: "maintenance_required", Details: "controlled mode can change only while stopped"}
		}
		if i.admission.uses != 0 || len(i.admission.maintenance) != 1 {
			return BusyError()
		}
		i.admission.transition = true
	}
	return nil
}

func (m *Maintenance) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.valid(); err != nil {
		return err
	}
	if err := m.checkPolicy(); err != nil {
		return err
	}
	if m.instance.process == nil {
		return fmt.Errorf("remote instances cannot be started locally")
	}
	return m.instance.process.startPermit(m.permit)
}

func (m *Maintenance) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.valid(); err != nil {
		return err
	}
	return m.instance.stop()
}

func (m *Maintenance) Restart() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.valid(); err != nil {
		return err
	}
	if m.instance.process == nil {
		return fmt.Errorf("remote instances cannot be restarted locally")
	}
	if err := m.checkPolicy(); err != nil {
		return err
	}
	return m.instance.process.restart(m)
}

// Retire prevents stale references from starting or proxying a deleted instance.
func (m *Maintenance) Retire() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.valid(); err != nil {
		return err
	}
	i := m.instance
	i.admission.mu.Lock()
	defer i.admission.mu.Unlock()
	if i.GetStatus() != Stopped && i.GetStatus() != Failed {
		return fmt.Errorf("stop instance before deleting")
	}
	i.admission.retired = true
	return nil
}

// StartAutomatic never starts controlled instances, including a legacy restart
// already scheduled before a stopped-mode transition.
func (i *Instance) StartAutomatic() error {
	m, err := i.BeginWorkMaintenance()
	if err != nil {
		return err
	}
	defer m.Close()
	if i.IsControlled() {
		return &AdmissionError{Code: "instance_not_running", Details: "controlled instances require explicit start"}
	}
	return m.Start()
}

// AcquireAutomaticUse reserves legacy use before the handler's eviction/start
// sequence. A stopped-mode transition cannot turn that already-admitted legacy
// request into a controlled request after it has evicted another instance.
func (i *Instance) AcquireAutomaticUse() (func(), error) {
	permit, err := i.policy.Acquire()
	if err != nil {
		return nil, err
	}
	release, err := i.acquireAutomaticInstanceUse()
	if err != nil {
		permit.Close()
		return nil, err
	}
	return func() { release(); permit.Close() }, nil
}

func (i *Instance) acquireAutomaticInstanceUse() (func(), error) {
	i.admission.mu.Lock()
	defer i.admission.mu.Unlock()
	if i.admission.retired || i.admission.transition {
		return nil, BusyError()
	}
	if i.IsControlled() {
		if len(i.admission.maintenance) != 0 {
			return nil, BusyError()
		}
		return nil, &AdmissionError{Code: "instance_not_running", Details: "controlled instances are use-running-only"}
	}
	i.admission.uses++
	var once sync.Once
	return func() { once.Do(func() { i.admission.mu.Lock(); i.admission.uses--; i.admission.mu.Unlock() }) }, nil
}

func (i *Instance) acquireUse(readOnly bool) (func(), bool, error) {
	var permit *resourcepolicy.Permit
	if !readOnly {
		var err error
		permit, err = i.policy.Acquire()
		if err != nil {
			return nil, i.IsControlled(), err
		}
	}
	release, controlled, err := i.acquireInstanceUse(readOnly)
	if err != nil {
		permit.Close()
		return nil, controlled, err
	}
	return func() { release(); permit.Close() }, controlled, nil
}

func (i *Instance) acquireInstanceUse(readOnly bool) (func(), bool, error) {
	i.admission.mu.Lock()
	defer i.admission.mu.Unlock()
	controlled := i.IsControlled()
	if i.admission.retired || i.admission.transition {
		return nil, controlled, BusyError()
	}
	if controlled {
		if !readOnly && len(i.admission.maintenance) != 0 {
			return nil, true, BusyError()
		}
		if !i.IsRunning() {
			return nil, true, &AdmissionError{Code: "instance_not_running", Details: "controlled instances are use-running-only; explicitly start first"}
		}
		if readOnly {
			return func() {}, true, nil
		}
	}
	i.admission.uses++
	return func() { i.admission.mu.Lock(); i.admission.uses--; i.admission.mu.Unlock() }, controlled, nil
}
