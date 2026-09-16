package manifest

import "testing"

func TestValidate_RefusesAPathTheIngressCannotExpress(t *testing.T) {
	cases := map[string]*RouteSpec{
		"internal path is relative":    {InternalPaths: []string{"admin"}},
		"internal path ends the rule":  {InternalPaths: []string{"/ad`min"}},
		"internal path is whole route": {InternalPaths: []string{"/"}},
		"public path is relative":      {PublicPaths: []string{"actuator/prometheus"}},
	}
	for name, route := range cases {
		t.Run(name, func(t *testing.T) {
			m := &Manifest{
				Project: "acme", Environment: "test",
				Apps: map[string]AppSpec{"api": {Image: "nginx:1", Port: 80, Route: route}},
			}
			if err := Validate(m); err == nil {
				t.Fatal("Validate = nil, want an error naming the app and the path")
			}
		})
	}
}

func TestValidate_AcceptsAWorkableRoute(t *testing.T) {
	m := &Manifest{
		Project: "acme", Environment: "test",
		Apps: map[string]AppSpec{"api": {Image: "nginx:1", Port: 80, Route: &RouteSpec{
			InternalPaths: []string{"/admin", "/internal/"},
			PublicPaths:   []string{"/actuator/prometheus"},
		}}},
	}
	if err := Validate(m); err != nil {
		t.Fatalf("Validate = %v, want nil", err)
	}
}
