package snap

import "testing"

func TestFreeFloor(t *testing.T) {
	const gb = 1 << 30
	for _, c := range []struct{ total, want int64 }{
		{16 * gb, 2 * gb},    // small disk: the 2 GB minimum
		{200 * gb, 10 * gb},  // 5%
		{4000 * gb, 20 * gb}, // big disk: capped at 20 GB
	} {
		if got := FreeFloor(c.total, Config{}); got != c.want {
			t.Errorf("FreeFloor(%d GB) = %d, want %d", c.total/gb, got, c.want)
		}
	}
	if got := FreeFloor(100*gb, Config{FreeFloorBytes: 3 * gb}); got != 3*gb {
		t.Errorf("configured floor ignored: %d", got)
	}
	sp := Space{Free: 10 * gb, Floor: 4 * gb}
	if !sp.Allows(6*gb) || sp.Allows(6*gb+1) {
		t.Error("Allows is off by one")
	}
}
