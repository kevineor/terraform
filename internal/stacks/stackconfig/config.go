package stackconfig

import (
	"fmt"
	"strings"

	"github.com/apparentlymart/go-versions/versions"
	"github.com/apparentlymart/go-versions/versions/constraints"
	"github.com/hashicorp/go-slug/sourceaddrs"
	"github.com/hashicorp/go-slug/sourcebundle"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/stacks/stackconfig/internal/typeexpr"
	"github.com/hashicorp/terraform/internal/tfdiags"
	"github.com/zclconf/go-cty/cty"
)

// maxEmbeddedStackNesting is an arbitrary, hopefully-reasonable limit on
// how much embedded stack nesting is allowed in a stack configuration.
//
// This is here to avoid unbounded resource usage for configurations with
// mistakes such as self-referencing source addresses or call cycles.
const maxEmbeddedStackNesting = 20

// Config represents an overall stack configuration tree, consisting of a
// root stack that might optionally have embedded stacks inside it, and
// so on for arbitrary levels of nesting.
type Config struct {
	Root *ConfigNode
}

// ConfigNode represents a node in a tree of stacks that are to be planned and
// applied together.
//
// A fully-resolved stack configuration has a root node of this type, which
// can have zero or more child nodes that are also of this type, and so on
// to arbitrary levels of nesting.
type ConfigNode struct {
	// Stack is the definition of this node in the stack tree.
	Stack *Stack

	// Children describes all of the embedded stacks nested directly beneath
	// this node in the stack tree. The keys match the labels on the "stack"
	// blocks in the configuration that [Config.Stack] was built from, and
	// so also match the keys in the EmbeddedStacks field of that Stack.
	Children map[string]*ConfigNode
}

// LoadConfigDir loads, parses, decodes, and partially-validates the
// stack configuration rooted at the given source address.
//
// If the given source address is a [sourceaddrs.LocalSource] then it is
// interpreted relative to the current process working directory. If it's
// a remote our registry source address then LoadConfigDir will attempt
// to read it from the provided source bundle.
//
// LoadConfigDir follows calls to embedded stacks and recursively loads
// those too, using the same source bundle for any non-local sources.
func LoadConfigDir(sourceAddr sourceaddrs.FinalSource, sources *sourcebundle.Bundle) (*Config, tfdiags.Diagnostics) {
	rootNode, diags := loadConfigDir(sourceAddr, sources, make([]sourceaddrs.FinalSource, 0, 3))
	if rootNode == nil {
		if !diags.HasErrors() {
			panic("LoadConfigDir returned no root node and no errors")
		}
		return nil, diags
	}

	ret := &Config{
		Root: rootNode,
	}

	// Before we return we need to walk the tree and resolve all of the
	// input variable and output value type constraints. This needs to happen
	// late in the process so that we can recognize provider local names in
	// the type constraints, and use consistent type objects for the same
	// provider across all nodes in the tree.
	diags = diags.Append(
		decodeTypeConstraints(ret),
	)

	return ret, diags
}

func loadConfigDir(sourceAddr sourceaddrs.FinalSource, sources *sourcebundle.Bundle, callers []sourceaddrs.FinalSource) (*ConfigNode, tfdiags.Diagnostics) {
	stack, diags := LoadSingleStackConfig(sourceAddr, sources)
	if stack == nil {
		if !diags.HasErrors() {
			panic("LoadSingleStackConfig returned no root node and no errors")
		}
		return nil, diags
	}

	ret := &ConfigNode{
		Stack:    stack,
		Children: make(map[string]*ConfigNode),
	}
	for _, call := range stack.EmbeddedStacks {
		effectiveSourceAddr, err := resolveFinalSourceAddr(sourceAddr, call.SourceAddr, call.VersionConstraints, sources)
		if err != nil {
			diags = diags.Append(&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Invalid source address",
				Detail: fmt.Sprintf(
					"Cannot use %q as a source address here: %s.",
					call.SourceAddr, err,
				),
				Subject: call.SourceAddrRange.ToHCL().Ptr(),
			})
			continue
		}

		if len(callers) == maxEmbeddedStackNesting {
			var callersBuf strings.Builder
			for i, addr := range callers {
				fmt.Fprintf(&callersBuf, "\n  %2d: %s", i+1, addr)
			}
			diags = diags.Append(&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Too much embedded stack nesting",
				Detail: fmt.Sprintf(
					"This embedded stack call is nested %d levels deep, which is greater than Terraform's nesting safety limit.\n\nWe recommend keeping stack configuration trees relatively flat, ideally using composition of a flat set of nested calls at the root.\n\nEmbedded stacks leading to this point:%s",
					len(callers), callersBuf.String(),
				),
				Subject: call.DeclRange.ToHCL().Ptr(),
			})
			continue
		}

		childNode, moreDiags := loadConfigDir(effectiveSourceAddr, sources, append(callers, sourceAddr))
		diags = diags.Append(moreDiags)
		if childNode != nil {
			ret.Children[call.Name] = childNode
		}
	}

	// We'll also populate the FinalSourceAddr field on each component,
	// so that callers can know the final absolute address of this
	// component's root module without having to retrace through our
	// recursive process here.
	for _, cmpn := range stack.Components {
		effectiveSourceAddr, err := resolveFinalSourceAddr(sourceAddr, cmpn.SourceAddr, cmpn.VersionConstraints, sources)
		if err != nil {
			diags = diags.Append(&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Invalid source address",
				Detail: fmt.Sprintf(
					"Cannot use %q as a source address here: %s.",
					cmpn.SourceAddr, err,
				),
				Subject: cmpn.SourceAddrRange.ToHCL().Ptr(),
			})
			continue
		}

		cmpn.FinalSourceAddr = effectiveSourceAddr
	}

	return ret, diags
}

func resolveFinalSourceAddr(base sourceaddrs.FinalSource, rel sourceaddrs.Source, versionConstraints constraints.IntersectionSpec, sources *sourcebundle.Bundle) (sourceaddrs.FinalSource, error) {
	switch rel := rel.(type) {
	case sourceaddrs.FinalSource:
		switch base := base.(type) {
		case sourceaddrs.RegistrySourceFinal:
			// This case is awkward because we'd ideally like to return
			// another registry source address in the same registry package
			// as base, but that might not actually be possible if "rel"
			// is a local source that traverses up out of the scope of
			// the registry package and into other parts of the real
			// underlying package. Therefore we'll first try the ideal
			// case but then do some more complex finagling if it fails.
			ret, err := sourceaddrs.ResolveRelativeFinalSource(base, rel)
			if err == nil {
				return ret, nil
			}

			// If we can't resolve relative to the registry source then
			// we need to resolve relative to its underlying remote source
			// instead.
			underlyingSource, ok := sources.RegistryPackageSourceAddr(base.Package(), base.SelectedVersion())
			if !ok {
				// If we also can't find the underlying source for some reason
				// then we're stuck.
				return nil, fmt.Errorf("can't find underlying source address for %s", base.Package())
			}
			underlyingSource = base.FinalSourceAddr(underlyingSource)
			return sourceaddrs.ResolveRelativeFinalSource(underlyingSource, rel)

		default:
			// Easy case: this source type is already a final type
			return sourceaddrs.ResolveRelativeFinalSource(base, rel)
		}
	case sourceaddrs.RegistrySource:
		// Registry sources are more annoying because we need to figure out
		// exactly which version the given version constraints select, which
		// we infer by what's available in the source bundle on the assumption
		// that the source bundler also selected the latest available version
		// that meets the given constraints.
		allowedVersions := versions.MeetingConstraints(versionConstraints)
		availableVersions := sources.RegistryPackageVersions(rel.Package())
		selectedVersion := availableVersions.NewestInSet(allowedVersions)
		if selectedVersion == versions.Unspecified {
			// We should get here only if the source bundle was built
			// incorrectly. A valid source bundle should always contain
			// at least one entry that matches each version constraint.
			return nil, fmt.Errorf("no cached versions of %s match the given version constraints", rel.Package())
		}
		finalRel := rel.Versioned(selectedVersion)
		return sourceaddrs.ResolveRelativeFinalSource(base, finalRel)
	default:
		// Should not get here because the above cases should be exhaustive
		// for all implementations of sourceaddrs.Source.
		return nil, fmt.Errorf("cannot resolve final source address for %T (this is a bug in Terraform)", rel)
	}
}

// decodeTypeConstraints handles the just-in-time postprocessing we do before
// returning from [LoadConfigDir], making sure that the type constraints
// on input variables and output values throughout the configuration are
// valid and consistent.
func decodeTypeConstraints(config *Config) tfdiags.Diagnostics {
	return decodeTypeConstraintsSingle(config.Root, make(map[addrs.Provider]cty.Type))
}

func decodeTypeConstraintsSingle(node *ConfigNode, types map[addrs.Provider]cty.Type) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics

	typeInfo := &decodeTypeConstraintsTypeInfo{
		types: types,
		reqs:  node.Stack.RequiredProviders,
	}
	for _, c := range node.Stack.InputVariables {
		diags = diags.Append(
			decodeTypeConstraint(&c.Type, typeInfo),
		)
	}
	for _, c := range node.Stack.OutputValues {
		diags = diags.Append(
			decodeTypeConstraint(&c.Type, typeInfo),
		)
	}

	for _, child := range node.Children {
		diags = diags.Append(
			decodeTypeConstraintsSingle(child, types),
		)
	}

	return diags
}

func decodeTypeConstraint(c *TypeConstraint, typeInfo *decodeTypeConstraintsTypeInfo) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics
	ty, defaults, hclDiags := typeexpr.TypeConstraint(c.Expression, typeInfo)
	c.Constraint = ty
	c.Defaults = defaults
	diags = diags.Append(hclDiags)
	return diags
}

type decodeTypeConstraintsTypeInfo struct {
	types map[addrs.Provider]cty.Type
	reqs  *ProviderRequirements
}

var _ typeexpr.TypeInformation = (*decodeTypeConstraintsTypeInfo)(nil)

// ProviderConfigType implements typeexpr.TypeInformation
func (ti *decodeTypeConstraintsTypeInfo) ProviderConfigType(providerAddr addrs.Provider) cty.Type {
	return ti.types[providerAddr]
}

// ProviderForLocalName implements typeexpr.TypeInformation
func (ti *decodeTypeConstraintsTypeInfo) ProviderForLocalName(localName string) (addrs.Provider, bool) {
	if ti.reqs == nil {
		return addrs.Provider{}, false
	}
	return ti.reqs.ProviderForLocalName(localName)
}

// SetProviderConfigType implements typeexpr.TypeInformation
func (ti *decodeTypeConstraintsTypeInfo) SetProviderConfigType(providerAddr addrs.Provider, ty cty.Type) {
	ti.types[providerAddr] = ty
}
