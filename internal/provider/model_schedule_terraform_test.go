package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// This uses a real Terraform process and a local management API, so every step
// checks the post-apply plan without provisioning a Baseten model or using keys.
func TestModelSchedulesTerraformLifecycle(t *testing.T) {
	terraformPath, err := exec.LookPath("terraform")
	if err != nil {
		t.Skip("terraform is needed for the offline lifecycle test")
	}
	t.Setenv("TF_ACC_TERRAFORM_PATH", terraformPath)
	var mu sync.Mutex
	var environment map[string]any
	encoded, err := json.Marshal(testAPIEnvironment(t, "production", 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &environment); err != nil {
		t.Fatal(err)
	}
	collection := map[string]any{"timezone": "UTC", "schedules": []any{}}
	environment["autoscaling_schedules"] = collection
	environment["request_backpressure_settings"] = map[string]any{}
	createdSchedules := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var response any
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/models", "GET /v1/models/model-1":
			model := map[string]any{"id": "model-1", "name": "whisper", "team_name": "Default Team", "created_at": "2026-01-01T00:00:00Z"}
			response = model
			if r.URL.Path == "/v1/models" {
				response = map[string]any{"models": []any{model}}
			}
		case "GET /v1/models/model-1/environments":
			response = map[string]any{"environments": []any{environment}}
		case "GET /v1/models/model-1/environments/production":
			response = environment
		case "PATCH /v1/models/model-1/environments/production":
			var update map[string]any
			if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			if value, ok := update["request_backpressure_settings"]; ok {
				environment["request_backpressure_settings"] = value
			}
			if value, ok := update["autoscaling_settings"].(map[string]any); ok {
				for key, setting := range value {
					environment["autoscaling_settings"].(map[string]any)[key] = setting
				}
			}
			if value, ok := update["autoscaling_schedule_settings"].(map[string]any); ok {
				if timezone, present := value["timezone"]; present {
					collection["timezone"] = timezone
				}
				schedules := collection["schedules"].([]any)
				deleted := map[string]bool{}
				if ids, ok := value["delete_schedules"].([]any); ok {
					for _, id := range ids {
						deleted[id.(string)] = true
					}
				}
				kept := make([]any, 0, len(schedules))
				for _, item := range schedules {
					if !deleted[item.(map[string]any)["id"].(string)] {
						kept = append(kept, item)
					}
				}
				if upserts, ok := value["schedules"].([]any); ok {
					for _, item := range upserts {
						schedule := item.(map[string]any)
						id, hasID := schedule["id"]
						if !hasID {
							createdSchedules++
							id = fmt.Sprintf("schedule-%d", createdSchedules)
							schedule["id"] = id
						}
						replaced := false
						for i, prior := range kept {
							if prior.(map[string]any)["id"] == id {
								kept[i] = schedule
								replaced = true
								break
							}
						}
						if !replaced {
							kept = append(kept, schedule)
						}
					}
				}
				collection["schedules"] = kept
			}
			response = map[string]any{"status": "ACCEPTED", "message": "updated", "environment": environment}
		default:
			t.Errorf("unexpected management API request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	config := func(settings string) string {
		return fmt.Sprintf(`
provider "baseten" {
  api_key = "test-key"
  remote_url = %q
}
resource "baseten_model" "test" {
  name = "whisper"
  environments = { production = { %s } }
}
`, server.URL, settings)
	}
	schedules := func(min int, enabled bool, includeEvent bool) string {
		event := ""
		if includeEvent {
			event = `event = {
  cadence = "ONE_TIME"
  enabled = true
  start_at = "2030-01-01T09:00:00Z"
  end_at = "2030-01-01T10:00:00Z"
  autoscaling = { min_replica = 1, max_replica = 4 }
}`
		}
		return fmt.Sprintf(`
request_backpressure_policy = "REJECT_ON_FULL"
autoscaling_schedule_timezone = "UTC"
autoscaling_schedules = {
  business = {
    cadence = "DAILY"
    enabled = %t
    weekdays = ["MONDAY", "TUESDAY"]
    start_hour = 9
    end_hour = 17
    start_minute = 0
    end_minute = 0
    autoscaling = { min_replica = %d, max_replica = 4 }
  }
  %s
}`, enabled, min, event)
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{Config: config(schedules(1, true, true)), Check: resource.TestCheckResourceAttr("baseten_model.test", "environments.production.autoscaling_schedules.business.id", "schedule-1")},
			{Config: config(schedules(2, false, false)), Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("baseten_model.test", "environments.production.autoscaling_schedules.business.id", "schedule-1"),
				resource.TestCheckResourceAttr("baseten_model.test", "environments.production.autoscaling_schedules.%", "1"),
			)},
			{Config: config(`autoscaling = { min_replica = 1 }`), Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("baseten_model.test", "environments.production.autoscaling_schedules.business.id", "schedule-1"),
				resource.TestCheckResourceAttr("baseten_model.test", "environments.production.request_backpressure_policy", "REJECT_ON_FULL"),
			)},
			{Config: config(`
autoscaling_schedules = {}
autoscaling_schedule_timezone = ""
request_backpressure_policy = ""
`), Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("baseten_model.test", "environments.production.autoscaling_schedules.%", "0"),
				resource.TestCheckResourceAttr("baseten_model.test", "environments.production.request_backpressure_policy", ""),
			)},
		},
	})
	mu.Lock()
	defer mu.Unlock()
	if createdSchedules != 2 {
		t.Fatalf("created %d schedules, want exactly two across all updates", createdSchedules)
	}
}
