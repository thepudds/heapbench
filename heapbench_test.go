package main

import (
	"testing"
	"unsafe" // for unsafe.Sizeof
)

func TestAddLiveMem(t *testing.T) {
	tests := []struct {
		createNodes  int
		wantNodes    int
		wantSliceLen int
	}{
		{1, 1, 1},
		{9, 9, 1},
		{10, 10, 1},
		{11, 11, 2},
		{99, 99, 10},
		{1001, 1001, 101},
	}
	for _, tt := range tests {
		t.Run("", func(t *testing.T) {
			var liveMem liveMemory
			liveMem.add(memBytes(tt.createNodes * int(unsafe.Sizeof(node{}))))
			gotNodes := 0
			for i := range liveMem.nodes {
				curr := liveMem.nodes[i]
				for curr != nil {
					gotNodes++
					curr = curr.next
				}
			}
			if gotNodes != tt.wantNodes {
				t.Errorf("got %d nodes, want %d", gotNodes, tt.wantNodes)
			}
			if len(liveMem.nodes) != tt.wantSliceLen {
				t.Errorf("len(liveMem.nodes) = %d, want %d", len(liveMem.nodes), tt.wantSliceLen)
			}
		})
	}
}
