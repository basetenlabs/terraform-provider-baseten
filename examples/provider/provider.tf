terraform {
  required_providers {
    baseten = {
      source  = "basetenlabs/baseten"
      version = "~> 0.1"
    }
  }
}

provider "baseten" {
  # Reads BASETEN_API_KEY when unset. Set it here only from a variable, since a
  # literal key ends up in version control.
  # api_key = var.baseten_api_key

  # Reads BASETEN_REMOTE_URL when unset, then https://api.baseten.co.
  # remote_url = "https://api.baseten.co"
}
