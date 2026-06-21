// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package terraform

import (
	"log"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"

	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/configs"
)

// TestModuleTransformer adds install nodes for the alternative modules
// referenced by "module" blocks inside test run blocks.
//
// Those modules act as the configuration under test for their run block, so
// they (and their descendant modules) must be installed and resolved just like
// the root module. Routing them through the init graph, rather than loading
// them statically, is what lets their descendant modules use dynamic
// (expression-based) source addresses.
//
// Each installed module is attached as a child of the root configuration under
// a synthetic single-segment key (see configs.TestRunModulePath). The configs
// package later detaches it from the root and records it as the run block's
// configuration under test.
type TestModuleTransformer struct {
	Config *configs.Config
	Walker configs.ModuleWalker
}

func (t *TestModuleTransformer) Transform(g *Graph) error {
	if t.Config == nil {
		return nil
	}

	// Only the root module's test files contribute run blocks. Nested modules
	// installed during the walk are loaded without their test files, but we
	// guard against acting on anything other than the root just in case.
	if t.Config.Parent != nil {
		return nil
	}

	for name, file := range t.Config.Module.Tests {
		for _, run := range file.Runs {
			if run.Module == nil || run.Module.Source == nil {
				continue
			}

			modPath := configs.TestRunModulePath(name, run.Name)
			key := modPath[len(modPath)-1]
			instancePath := g.Path.Child(key, addrs.NoKey)

			// Synthesize a module call so we can reuse nodeInstallModule. The
			// call's name is the run name (a valid identifier used for
			// diagnostics and the module manifest), while the synthetic key
			// distinguishes this run's module within the root configuration.
			//
			// The source (and version) of a test run "module" block is always
			// a static literal, so we can rebuild equivalent expressions from
			// the already-parsed values rather than retaining the originals.
			call := &configs.ModuleCall{
				Name:       run.Name,
				SourceExpr: hcl.StaticExpr(cty.StringVal(run.Module.Source.String()), run.Module.SourceDeclRange),
				Config:     hcl.EmptyBody(),
				DeclRange:  run.Module.DeclRange,
			}
			if len(run.Module.Version.Required) > 0 {
				call.VersionExpr = hcl.StaticExpr(cty.StringVal(run.Module.Version.Required.String()), run.Module.Version.DeclRange)
			}

			n := &nodeInstallModule{
				Addr:       instancePath,
				ModuleCall: call,
				Parent:     t.Config,
				Walker:     t.Walker,
			}
			g.Add(n)
			log.Printf("[TRACE] TestModuleTransformer: Added %s for run %q in %s", instancePath, run.Name, name)
		}
	}

	return nil
}
