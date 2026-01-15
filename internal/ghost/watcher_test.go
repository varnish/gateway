package ghost

import (
	"testing"

	discoveryv1 "k8s.io/api/discovery/v1"
)

func TestDiffEndpoints(t *testing.T) {
	tests := []struct {
		name         string
		oldEndpoints []Endpoint
		newEndpoints []Endpoint
		wantAdded    int
		wantRemoved  int
	}{
		{
			name:         "empty old and new",
			oldEndpoints: []Endpoint{},
			newEndpoints: []Endpoint{},
			wantAdded:    0,
			wantRemoved:  0,
		},
		{
			name:         "nil old and new",
			oldEndpoints: nil,
			newEndpoints: nil,
			wantAdded:    0,
			wantRemoved:  0,
		},
		{
			name:         "empty old, non-empty new",
			oldEndpoints: []Endpoint{},
			newEndpoints: []Endpoint{
				{IP: "10.0.0.1", Port: 8080},
				{IP: "10.0.0.2", Port: 8080},
			},
			wantAdded:   2,
			wantRemoved: 0,
		},
		{
			name: "non-empty old, empty new",
			oldEndpoints: []Endpoint{
				{IP: "10.0.0.1", Port: 8080},
				{IP: "10.0.0.2", Port: 8080},
			},
			newEndpoints: []Endpoint{},
			wantAdded:    0,
			wantRemoved:  2,
		},
		{
			name: "same endpoints no changes",
			oldEndpoints: []Endpoint{
				{IP: "10.0.0.1", Port: 8080},
				{IP: "10.0.0.2", Port: 8080},
			},
			newEndpoints: []Endpoint{
				{IP: "10.0.0.1", Port: 8080},
				{IP: "10.0.0.2", Port: 8080},
			},
			wantAdded:   0,
			wantRemoved: 0,
		},
		{
			name: "some added some removed",
			oldEndpoints: []Endpoint{
				{IP: "10.0.0.1", Port: 8080},
				{IP: "10.0.0.2", Port: 8080},
			},
			newEndpoints: []Endpoint{
				{IP: "10.0.0.2", Port: 8080},
				{IP: "10.0.0.3", Port: 8080},
			},
			wantAdded:   1, // 10.0.0.3
			wantRemoved: 1, // 10.0.0.1
		},
		{
			name: "port change counts as add and remove",
			oldEndpoints: []Endpoint{
				{IP: "10.0.0.1", Port: 8080},
			},
			newEndpoints: []Endpoint{
				{IP: "10.0.0.1", Port: 9090},
			},
			wantAdded:   1,
			wantRemoved: 1,
		},
		{
			name: "all replaced",
			oldEndpoints: []Endpoint{
				{IP: "10.0.0.1", Port: 8080},
				{IP: "10.0.0.2", Port: 8080},
			},
			newEndpoints: []Endpoint{
				{IP: "10.0.0.3", Port: 8080},
				{IP: "10.0.0.4", Port: 8080},
			},
			wantAdded:   2,
			wantRemoved: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			added, removed := diffEndpoints(tt.oldEndpoints, tt.newEndpoints)
			if len(added) != tt.wantAdded {
				t.Errorf("added: got %d, want %d", len(added), tt.wantAdded)
			}
			if len(removed) != tt.wantRemoved {
				t.Errorf("removed: got %d, want %d", len(removed), tt.wantRemoved)
			}
		})
	}
}

func TestDiffEndpointsContent(t *testing.T) {
	old := []Endpoint{
		{IP: "10.0.0.1", Port: 8080},
		{IP: "10.0.0.2", Port: 8080},
	}
	new := []Endpoint{
		{IP: "10.0.0.2", Port: 8080},
		{IP: "10.0.0.3", Port: 8080},
	}

	added, removed := diffEndpoints(old, new)

	// Check added contains the right endpoint
	if len(added) != 1 {
		t.Fatalf("expected 1 added, got %d", len(added))
	}
	if added[0].IP != "10.0.0.3" || added[0].Port != 8080 {
		t.Errorf("unexpected added endpoint: %v", added[0])
	}

	// Check removed contains the right endpoint
	if len(removed) != 1 {
		t.Fatalf("expected 1 removed, got %d", len(removed))
	}
	if removed[0].IP != "10.0.0.1" || removed[0].Port != 8080 {
		t.Errorf("unexpected removed endpoint: %v", removed[0])
	}
}

func TestExtractEndpoints(t *testing.T) {
	boolPtr := func(b bool) *bool { return &b }
	int32Ptr := func(i int32) *int32 { return &i }

	tests := []struct {
		name      string
		slice     *discoveryv1.EndpointSlice
		wantCount int
	}{
		{
			name: "single ready endpoint single address",
			slice: &discoveryv1.EndpointSlice{
				Endpoints: []discoveryv1.Endpoint{
					{
						Addresses:  []string{"10.0.0.1"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
					},
				},
				Ports: []discoveryv1.EndpointPort{
					{Port: int32Ptr(8080)},
				},
			},
			wantCount: 1,
		},
		{
			name: "single endpoint multiple addresses",
			slice: &discoveryv1.EndpointSlice{
				Endpoints: []discoveryv1.Endpoint{
					{
						Addresses:  []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
					},
				},
				Ports: []discoveryv1.EndpointPort{
					{Port: int32Ptr(8080)},
				},
			},
			wantCount: 3,
		},
		{
			name: "multiple endpoints mixed ready states",
			slice: &discoveryv1.EndpointSlice{
				Endpoints: []discoveryv1.Endpoint{
					{
						Addresses:  []string{"10.0.0.1"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
					},
					{
						Addresses:  []string{"10.0.0.2"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(false)},
					},
					{
						Addresses:  []string{"10.0.0.3"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
					},
				},
				Ports: []discoveryv1.EndpointPort{
					{Port: int32Ptr(8080)},
				},
			},
			wantCount: 2, // only ready ones
		},
		{
			name: "endpoint with nil ready treated as ready",
			slice: &discoveryv1.EndpointSlice{
				Endpoints: []discoveryv1.Endpoint{
					{
						Addresses:  []string{"10.0.0.1"},
						Conditions: discoveryv1.EndpointConditions{Ready: nil},
					},
				},
				Ports: []discoveryv1.EndpointPort{
					{Port: int32Ptr(8080)},
				},
			},
			wantCount: 1,
		},
		{
			name: "empty endpoints list",
			slice: &discoveryv1.EndpointSlice{
				Endpoints: []discoveryv1.Endpoint{},
				Ports: []discoveryv1.EndpointPort{
					{Port: int32Ptr(8080)},
				},
			},
			wantCount: 0,
		},
		{
			name: "no ports defaults to port 0",
			slice: &discoveryv1.EndpointSlice{
				Endpoints: []discoveryv1.Endpoint{
					{
						Addresses:  []string{"10.0.0.1"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
					},
				},
				Ports: []discoveryv1.EndpointPort{},
			},
			wantCount: 1,
		},
		{
			name: "nil port value defaults to port 0",
			slice: &discoveryv1.EndpointSlice{
				Endpoints: []discoveryv1.Endpoint{
					{
						Addresses:  []string{"10.0.0.1"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
					},
				},
				Ports: []discoveryv1.EndpointPort{
					{Port: nil},
				},
			},
			wantCount: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			endpoints := extractEndpoints(tt.slice)
			if len(endpoints) != tt.wantCount {
				t.Errorf("got %d endpoints, want %d", len(endpoints), tt.wantCount)
			}
		})
	}
}

func TestExtractEndpointsPortValue(t *testing.T) {
	int32Ptr := func(i int32) *int32 { return &i }
	boolPtr := func(b bool) *bool { return &b }

	slice := &discoveryv1.EndpointSlice{
		Endpoints: []discoveryv1.Endpoint{
			{
				Addresses:  []string{"10.0.0.1"},
				Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
			},
		},
		Ports: []discoveryv1.EndpointPort{
			{Port: int32Ptr(9090)},
		},
	}

	endpoints := extractEndpoints(slice)
	if len(endpoints) != 1 {
		t.Fatalf("expected 1 endpoint, got %d", len(endpoints))
	}
	if endpoints[0].Port != 9090 {
		t.Errorf("expected port 9090, got %d", endpoints[0].Port)
	}
	if endpoints[0].IP != "10.0.0.1" {
		t.Errorf("expected IP 10.0.0.1, got %s", endpoints[0].IP)
	}
}

func TestExtractEndpointsNoPorts(t *testing.T) {
	boolPtr := func(b bool) *bool { return &b }

	slice := &discoveryv1.EndpointSlice{
		Endpoints: []discoveryv1.Endpoint{
			{
				Addresses:  []string{"10.0.0.1"},
				Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
			},
		},
		Ports: []discoveryv1.EndpointPort{},
	}

	endpoints := extractEndpoints(slice)
	if len(endpoints) != 1 {
		t.Fatalf("expected 1 endpoint, got %d", len(endpoints))
	}
	if endpoints[0].Port != 0 {
		t.Errorf("expected port 0 (default), got %d", endpoints[0].Port)
	}
}
