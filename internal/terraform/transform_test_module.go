// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package terraform

import (
	"log"

	"github.com/hashicorp/hcl/v2"

	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/configs"
	"github.com/hashicorp/terraform/internal/lang/langrefs"
	"github.com/hashicorp/terraform/internal/tfdiags"
)

// TestModuleTransformer adds an install node for each "module" block declared
// in a test run block.
//
// Those alternative modules act as the configuration under test for their run
// block, so they (and their descendant modules) must be installed and resolved
// just like the root module. Routing them through the init graph, rather than
// loading them statically, is what lets their descendant modules use dynamic
// (expression-based) source addresses.
//
// The node represents the run as a whole: the run block carries both the
// module source and the input variable expressions, which is everything needed
// to install the module and walk its descendants. Once installed, the node
// records the result on the run's ConfigUnderTest; configs.FinalizeConfig later
// rebases it to act as the root of its own module tree.
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

			// A run module receives the test file's global variables as well as
			// the run block's own, with the run block taking precedence.
			variables := make(map[string]hcl.Expression, len(file.Variables)+len(run.Variables))
			for k, v := range file.Variables {
				variables[k] = v
			}
			for k, v := range run.Variables {
				variables[k] = v
			}

			n := &nodeInstallTestRunModule{
				Addr:      instancePath,
				Run:       run,
				Variables: variables,
				Parent:    t.Config,
				Walker:    t.Walker,
			}
			g.Add(n)
			log.Printf("[TRACE] TestModuleTransformer: Added %s for run %q in %s", instancePath, run.Name, name)
		}
	}

	return nil
}

// nodeInstallTestRunModule installs the alternative module of a single test run
// block and resolves its descendant modules through the init graph.
//
// It mirrors nodeInstallModule, but its "call site" is a run block rather than
// a module call: the source is already a parsed literal (run.Module.Source) and
// the input arguments come from the run block's (and test file's) variables.
type nodeInstallTestRunModule struct {
	// Addr is the synthetic single-segment module instance the run module is
	// installed under (e.g. test.main.setup), placing it directly beneath the
	// root module in the graph.
	Addr addrs.ModuleInstance
	Run  *configs.TestRun

	// Variables holds the input variable expressions supplied to the run
	// module: the test file's global variables merged with the run block's own.
	Variables map[string]hcl.Expression

	Parent *configs.Config
	Walker configs.ModuleWalker

	// Config stores the configuration of the installed module.
	Config *configs.Config
}

var (
	_ GraphNodeExecutable        = (*nodeInstallTestRunModule)(nil)
	_ GraphNodeReferencer        = (*nodeInstallTestRunModule)(nil)
	_ GraphNodeDynamicExpandable = (*nodeInstallTestRunModule)(nil)
	_ GraphNodeModuleInstance    = (*nodeInstallTestRunModule)(nil)
)

func (n *nodeInstallTestRunModule) Path() addrs.ModuleInstance {
	return n.Addr.Parent()
}

func (n *nodeInstallTestRunModule) Name() string {
	return n.Addr.String()
}

func (n *nodeInstallTestRunModule) ModulePath() addrs.Module {
	return n.Addr.Module().Parent()
}

func (n *nodeInstallTestRunModule) References() []*addrs.Reference {
	var refs []*addrs.Reference

	// The source and version of a run "module" block are always static
	// literals, so the only references we can contribute come from the input
	// variable expressions. Some of those may be used as constant variables to
	// build a descendant module's source, so wiring them creates the proper
	// dependency edges in the init graph.
	for _, expr := range n.Variables {
		inputRefs, _ := langrefs.ReferencesInExpr(addrs.ParseRef, expr)
		refs = append(refs, inputRefs...)
	}

	return refs
}

func (n *nodeInstallTestRunModule) Execute(ctx EvalContext, walkOp walkOperation) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics

	req := &configs.ModuleRequest{
		Name:              n.Run.Name,
		Path:              n.Addr.Module(),
		SourceAddr:        n.Run.Module.Source,
		SourceAddrRange:   n.Run.Module.SourceDeclRange,
		VersionConstraint: n.Run.Module.Version,
		Parent:            n.Parent,
		CallRange:         n.Run.Module.DeclRange,
	}

	cfg, ver, modDiags := n.Walker.LoadModule(req)
	diags = diags.Append(modDiags)
	if diags.HasErrors() {
		return diags
	}

	config := &configs.Config{
		Module:            cfg,
		Parent:            n.Parent,
		Path:              n.Addr.Module(),
		Root:              n.Parent.Root,
		Children:          map[string]*configs.Config{},
		CallRange:         n.Run.Module.DeclRange,
		SourceAddr:        n.Run.Module.Source,
		SourceAddrRaw:     n.Run.Module.Source.String(),
		SourceAddrRange:   n.Run.Module.SourceDeclRange,
		Version:           ver,
		VersionConstraint: n.Run.Module.Version,
	}

	// Attach the installed module under the root module, keyed by the synthetic
	// path segment. This is a walk-time scaffold: while descendant modules are
	// installed (in DynamicExpand), the evaluator resolves references within
	// this module by looking it up from the root config via DescendantForInstance,
	// so it must be reachable there. configs.FinalizeConfig (buildTestModules)
	// later detaches it and records it as the run's ConfigUnderTest.
	currentModuleKey := n.Addr[len(n.Addr)-1].Name
	n.Parent.Children[currentModuleKey] = config

	// During init, modules are loaded incrementally so the checks state built
	// at walk start only knows about the root module. Register all checkable
	// objects from the newly loaded module so that validation nodes added by
	// DynamicExpand can find their check entries.
	ctx.Checks().RegisterModule(config)

	n.Config = config

	return diags
}

func (n *nodeInstallTestRunModule) DynamicExpand(ctx EvalContext) (*Graph, tfdiags.Diagnostics) {
	var g Graph
	var diags tfdiags.Diagnostics

	if n.Config == nil {
		// Cannot expand without a config. This can happen when something goes
		// wrong during module installation/Execute() above.
		return nil, diags
	}

	expander := ctx.InstanceExpander()
	_, call := n.Addr.Call()
	expander.SetModuleSingle(n.Path(), call)

	graph, graphDiags := (&InitGraphBuilder{
		Config: n.Config,
		Walker: n.Walker,
		// The run module's call site is the run block, so its input variable
		// values come from the run/file variables rather than from a module
		// call block.
		CallExpressions: n.Variables,
	}).Build(n.Addr)
	diags = diags.Append(graphDiags)
	if graphDiags.HasErrors() {
		return nil, diags
	}
	g.Subsume(&graph.AcyclicGraph.Graph)

	addRootNodeToGraph(&g)

	return &g, nil
}
