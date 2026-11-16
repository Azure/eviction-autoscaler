package helm_test

import (
	"path/filepath"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"
)

func TestLeaderElectionRendering(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want []string
	}{
		{name: "defaults", want: []string{"60s", "40s", "2s"}},
		{
			name: "missing section",
			args: []string{"--set", "controllerConfig.leaderElection=null"},
			want: []string{"60s", "40s", "2s"},
		},
		{
			name: "overrides",
			args: []string{"--set", "controllerConfig.leaderElection.leaseDuration=90s",
				"--set", "controllerConfig.leaderElection.renewDeadline=60s",
				"--set", "controllerConfig.leaderElection.retryPeriod=3s"},
			want: []string{"90s", "60s", "3s"},
		},
		{
			name: "invalid durations reach startup validation",
			args: []string{"--set", "controllerConfig.leaderElection.renewDeadline=0s"},
			want: []string{"60s", "0s", "2s"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := []string{"template", "election-test",
				filepath.Join("..", "..", "helm", "eviction-autoscaler"),
				"--show-only", "templates/deployment.yaml"}
			output, err := runHelm(t, append(args, test.args...)...)
			if err != nil {
				t.Fatalf("render: %v\n%s", err, output)
			}
			var deployment appsv1.Deployment
			if err := yaml.Unmarshal(output, &deployment); err != nil {
				t.Fatal(err)
			}
			flags := []string{"lease-duration", "renew-deadline", "retry-period"}
			for i, name := range flags {
				want := "--leader-election-" + name + "=" + test.want[i]
				if !slices.Contains(deployment.Spec.Template.Spec.Containers[0].Args, want) {
					t.Errorf("missing argument %q", want)
				}
			}
		})
	}
}
