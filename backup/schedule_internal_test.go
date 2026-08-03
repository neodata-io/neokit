package backup

import (
	"context"
	"testing"
)

type noopSnap struct{}

func (noopSnap) SnapshotTo(context.Context, string) error { return nil }

// The configured/default/midnight time has to actually reach the schedule Run
// drives — this was previously checked by parsing the boot-report Detail
// string; with that gone, read the field New actually set.
func TestNewStoresTheConfiguredScheduleTime(t *testing.T) {
	svc := New(noopSnap{}, Options{Dir: t.TempDir(), Retention: 7})
	if svc.at != (Clock{Hour: DefaultHour}) {
		t.Errorf("at = %s, want the default %02d:00", svc.at, DefaultHour)
	}

	custom := Clock{Hour: 21, Minute: 30}
	svc2 := New(noopSnap{}, Options{Dir: t.TempDir(), Retention: 7, At: &custom})
	if svc2.at != custom {
		t.Errorf("at = %s, want the configured %s", svc2.at, custom)
	}

	midnight := Clock{}
	svc3 := New(noopSnap{}, Options{Dir: t.TempDir(), At: &midnight})
	if svc3.at != midnight {
		t.Errorf("at = %s, want midnight rather than the default", svc3.at)
	}
}
