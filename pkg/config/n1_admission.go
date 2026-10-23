package config

import "sync"

// AdmissionDomain is the single local N1 lane. It carries no process authority:
// owner permits and instance admission are still mandatory for every effect.
// No mutex is held across I/O; returned functions release counted reservations.
type AdmissionDomain struct {
	mu       sync.Mutex
	uses     int
	reserved bool
}

func (d *AdmissionDomain) Enter() (func(), bool) {
	if d == nil {
		return func() {}, true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.reserved {
		return nil, false
	}
	d.uses++
	var once sync.Once
	return func() { once.Do(func() { d.mu.Lock(); d.uses--; d.mu.Unlock() }) }, true
}
func (d *AdmissionDomain) Reserve() (func(), bool) {
	if d == nil {
		return func() {}, true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.reserved || d.uses != 0 {
		return nil, false
	}
	d.reserved = true
	var once sync.Once
	return func() { once.Do(func() { d.mu.Lock(); d.reserved = false; d.mu.Unlock() }) }, true
}
