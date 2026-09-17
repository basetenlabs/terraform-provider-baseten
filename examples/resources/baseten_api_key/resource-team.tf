# A key belongs to one team. Set team_name, or team_id if you have it, and set
# neither for the organization's default team. Names only have to be unique
# within the team, across every workspace key type.
resource "baseten_api_key" "research_inference" {
  name      = "inference"
  type      = "WORKSPACE_INVOKE"
  team_name = "research"
}
