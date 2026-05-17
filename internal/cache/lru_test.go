package cache

import "testing"

func TestLRUBasic(t *testing.T) {
	c := New[string, int](3)
	c.Add("a", 1)
	c.Add("b", 2)
	c.Add("c", 3)

	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Errorf("Get a = %v,%v want 1,true", v, ok)
	}
	if c.Len() != 3 {
		t.Errorf("Len = %d, want 3", c.Len())
	}
}

func TestLRUEviction(t *testing.T) {
	c := New[string, int](2)
	c.Add("a", 1)
	c.Add("b", 2)
	c.Add("c", 3) // evicts "a"

	if _, ok := c.Get("a"); ok {
		t.Error("expected a to be evicted")
	}
	if v, ok := c.Get("b"); !ok || v != 2 {
		t.Errorf("Get b = %v,%v want 2,true", v, ok)
	}
	if v, ok := c.Get("c"); !ok || v != 3 {
		t.Errorf("Get c = %v,%v want 3,true", v, ok)
	}
}

func TestLRUPromotion(t *testing.T) {
	c := New[string, int](2)
	c.Add("a", 1)
	c.Add("b", 2)
	// Touch "a" so "b" becomes the LRU.
	c.Get("a")
	c.Add("c", 3) // should evict "b" not "a"

	if _, ok := c.Get("b"); ok {
		t.Error("expected b to be evicted after promotion of a")
	}
	if _, ok := c.Get("a"); !ok {
		t.Error("expected a to survive")
	}
}

func TestLRUOverwrite(t *testing.T) {
	c := New[string, int](2)
	c.Add("a", 1)
	c.Add("a", 99)
	if v, _ := c.Get("a"); v != 99 {
		t.Errorf("overwrite failed: got %d, want 99", v)
	}
	if c.Len() != 1 {
		t.Errorf("overwrite should not grow Len: got %d, want 1", c.Len())
	}
}

func TestLRURemove(t *testing.T) {
	c := New[string, int](2)
	c.Add("a", 1)
	c.Remove("a")
	if _, ok := c.Get("a"); ok {
		t.Error("expected a to be removed")
	}
	c.Remove("nonexistent") // must not panic
}

func TestLRUUnbounded(t *testing.T) {
	c := New[string, int](0)
	for i := 0; i < 1000; i++ {
		c.Add(string(rune('a'+i%26)), i)
	}
	// Should have at most 26 entries (only 26 distinct keys).
	if c.Len() > 26 {
		t.Errorf("Len = %d, want <= 26", c.Len())
	}
	// Capacity 0 means no eviction even with many distinct keys.
	c2 := New[int, int](0)
	for i := 0; i < 1000; i++ {
		c2.Add(i, i)
	}
	if c2.Len() != 1000 {
		t.Errorf("Len = %d, want 1000 (no eviction when cap<=0)", c2.Len())
	}
}
