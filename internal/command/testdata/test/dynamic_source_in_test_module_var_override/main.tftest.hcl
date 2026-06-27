run "test" {
  module {
    source = "./testmod"
  }

  variables {
    # Override the default ("default_module") to verify that run.Variables
    # expressions are actually used when resolving the dynamic source.
    module_name = "other_module"
  }

  assert {
    condition     = module.mod.value == "from_other"
    error_message = "expected from_other: run.Variables override was not applied"
  }
}
