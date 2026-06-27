// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package terraform

import (
	"fmt"
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
//
// A synthetic ModuleCall is also registered in the root module's ModuleCalls
// map under the same key, carrying the variable expressions from the run
// block. This lets ModuleVariableTransformer find a call site for the module
// without any special-case handling.
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
			//
			// The config body carries the variable expressions from the run
			// block so that ModuleVariableTransformer can resolve them during
			// the init walk without any special-case handling. It also makes
			// nodeInstallModule.References() aware of those expressions, so
			// that any locals or variables they reference create proper
			// dependency edges in the init graph.
			call := &configs.ModuleCall{
				Name:       run.Name,
				SourceExpr: hcl.StaticExpr(cty.StringVal(run.Module.Source.String()), run.Module.SourceDeclRange),
				Config:     runVariableBody{exprs: run.Variables, rng: run.Module.DeclRange},
				DeclRange:  run.Module.DeclRange,
			}
			if len(run.Module.Version.Required) > 0 {
				call.VersionExpr = hcl.StaticExpr(cty.StringVal(run.Module.Version.Required.String()), run.Module.Version.DeclRange)
			}

			// Register the call in the root module so that
			// ModuleVariableTransformer can find a call site for this module
			// during the init sub-graph walk (DynamicExpand). The entry is
			// keyed by the synthetic path segment, not by run.Name, to match
			// what c.Path.Call() returns inside transformSingle.
			t.Config.Module.ModuleCalls[key] = call

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

// runVariableBody is an hcl.Body backed by the variable expressions from a
// test run block. It is used as the Config body of the synthetic ModuleCall
// registered for each test run module, so that ModuleVariableTransformer can
// read per-variable expressions the same way it does for regular module calls.
type runVariableBody struct {
	exprs map[string]hcl.Expression
	rng   hcl.Range
}

func (b runVariableBody) Content(schema *hcl.BodySchema) (*hcl.BodyContent, hcl.Diagnostics) {
	content, remain, diags := b.PartialContent(schema)
	remainB := remain.(runVariableBody)
	for name := range remainB.exprs {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Unsupported argument",
			Detail:   fmt.Sprintf("An argument named %q is not expected here.", name),
			Subject:  b.exprs[name].StartRange().Ptr(),
		})
	}
	return content, diags
}

func (b runVariableBody) PartialContent(schema *hcl.BodySchema) (*hcl.BodyContent, hcl.Body, hcl.Diagnostics) {
	var diags hcl.Diagnostics
	content := &hcl.BodyContent{
		Attributes:       make(hcl.Attributes),
		MissingItemRange: b.rng,
	}

	remain := make(map[string]hcl.Expression, len(b.exprs))
	for k, v := range b.exprs {
		remain[k] = v
	}

	for _, attrS := range schema.Attributes {
		delete(remain, attrS.Name)
		expr, defined := b.exprs[attrS.Name]
		if !defined {
			if attrS.Required {
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "Missing required argument",
					Detail:   fmt.Sprintf("The argument %q is required, but no definition was found.", attrS.Name),
					Subject:  b.rng.Ptr(),
				})
			}
			continue
		}
		rng := expr.StartRange()
		content.Attributes[attrS.Name] = &hcl.Attribute{
			Name:      attrS.Name,
			Expr:      expr,
			NameRange: rng,
			Range:     rng,
		}
	}

	return content, runVariableBody{exprs: remain, rng: b.rng}, diags
}

func (b runVariableBody) JustAttributes() (hcl.Attributes, hcl.Diagnostics) {
	ret := make(hcl.Attributes, len(b.exprs))
	for name, expr := range b.exprs {
		rng := expr.StartRange()
		ret[name] = &hcl.Attribute{
			Name:      name,
			Expr:      expr,
			NameRange: rng,
			Range:     rng,
		}
	}
	return ret, nil
}

func (b runVariableBody) MissingItemRange() hcl.Range {
	return b.rng
}
