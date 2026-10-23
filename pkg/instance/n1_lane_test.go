package instance

import (
	"context"
	"net/http"
	"testing"
	"time"

	"llamactl/pkg/config"
)

func TestN1GlobalLaneRejectsOtherNativeAndAutomaticEntry(t *testing.T) {
	first, _ := protocolFixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	publishSyntheticReady(t, first, "alpha")
	cfg, opts := controlledFixture(t)
	lane := &config.AdmissionDomain{}
	cfg.N1Lane = lane
	first.lane = lane
	other := New("other", cfg, opts, nil)
	s := first.ControlSnapshot()
	r, err := first.ReserveN1("run", "scope", "evaluation", s.BootID, s.Revision, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = other.BeginWorkMaintenance(); err == nil {
		t.Fatal("native start bypassed global lane")
	}
	if _, err = other.AcquireAutomaticUse(); err == nil {
		t.Fatal("automatic path bypassed global lane")
	}
	if _, _, err = other.acquireUse(false); err == nil {
		t.Fatal("inference bypassed global lane")
	}
	m, err := r.BeginControl(true)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.CheckControl(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	m.Close()
	if err = r.CloseConfirmed(); err != nil {
		t.Fatal(err)
	}
	m, err = other.BeginWorkMaintenance()
	if err != nil {
		t.Fatal(err)
	}
	m.Close()
}
