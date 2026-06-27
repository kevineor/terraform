run "test" {
  module {
    source = "./testmod"
  }

  # required_input is intentionally not supplied here to exercise the
  # "missing required argument" diagnostic path during terraform init.

  assert {
    condition     = output.value != null
    error_message = "unreachable"
  }
}
