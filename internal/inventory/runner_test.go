package inventory

import (
	"encoding/base64"
	"testing"
)

func TestParseRunnerDiscoverySafeMetadata(t *testing.T) {
	meta := base64.StdEncoding.EncodeToString([]byte(`{"agentName":"build-1","gitHubUrl":"https://github.com/acme/app","labels":[{"name":"self-hosted"},{"name":"linux"}],"workFolder":"_work"}`))
	raw := "CCM_RUNNER_DIR\t/home/github-runner/runner-1\n" +
		"CCM_RUNNER_UNIT\tactions.runner.acme-app.build-1.service\n" +
		"active\tenabled\t42\tFri 2026-07-17 10:00:00 AEST\tsuccess\t/home/github-runner/runner-1\t\n" +
		"CCM_RUNNER_META\tactions.runner.acme-app.build-1.service\t" + meta + "\n"
	runners, err := parseRunnerDiscovery("runner-vm", "/home/github-runner", raw)
	if err != nil || len(runners) != 1 {
		t.Fatalf("parse runners: %v %#v", err, runners)
	}
	r := runners[0]
	if r.RunnerName != "build-1" || r.GitHubURL == "" || len(r.Labels) != 2 || r.PID != 42 {
		t.Fatalf("unexpected runner: %#v", r)
	}
}
