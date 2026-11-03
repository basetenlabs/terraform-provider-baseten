resource "baseten_model" "scheduled" {
  name = "Phi 3 Mini"

  environments = {
    production = {
      request_backpressure_policy   = "REJECT_ON_FULL"
      autoscaling_schedule_timezone = "America/Los_Angeles"
      autoscaling_schedules = {
        business_hours = {
          cadence      = "DAILY"
          enabled      = true
          weekdays     = ["MONDAY", "TUESDAY", "WEDNESDAY", "THURSDAY", "FRIDAY"]
          start_hour   = 9
          start_minute = 0
          end_hour     = 17
          end_minute   = 0
          autoscaling = {
            min_replica = 2
            max_replica = 20
          }
        }
      }
    }
  }
}
