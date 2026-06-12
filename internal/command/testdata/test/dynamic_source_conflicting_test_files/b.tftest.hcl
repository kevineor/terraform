variables {
  module_name = "other"
}

run "validate_dynamic_module" {
  assert {
    condition     = module.mod.value == "baz"
    error_message = "expected baz from dynamically sourced module"
  }
}
