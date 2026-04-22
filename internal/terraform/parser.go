package terraform

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
	"github.com/zclconf/go-cty/cty/function/stdlib"
)

type StateLoader interface {
	Load(ctx context.Context, backend S3Backend) (*StateSummary, error)
	LoadForWorkspace(ctx context.Context, backend S3Backend, workspace string) (*StateSummary, error)
}

type Parser struct {
	stateLoader StateLoader
}

type terraformConfig struct {
	backend         *S3Backend
	resourceBlocks  []resourceBlock
	variableDefault map[string]cty.Value
	locals          map[string]hclsyntax.Expression
	tfvarsFiles     []tfvarsFile
}

type resourceBlock struct {
	file  string
	block *hclsyntax.Block
}

type tfvarsFile struct {
	name      string
	path      string
	variables map[string]cty.Value
}

func NewParser(stateLoader StateLoader) *Parser {
	return &Parser{stateLoader: stateLoader}
}

func (p *Parser) ParseRepository(ctx context.Context, root string) (*Analysis, error) {
	cfg, err := loadTerraformConfig(root)
	if err != nil {
		return nil, err
	}

	analysis := &Analysis{
		Backend: cfg.backend,
	}

	baseVars := cloneValueMap(cfg.variableDefault)
	baseLocals, err := evaluateLocals(cfg.locals, baseVars)
	if err != nil {
		return nil, err
	}
	baseResources, err := expandResources(cfg.resourceBlocks, buildEvalContext(baseVars, baseLocals))
	if err != nil {
		return nil, err
	}
	analysis.Resources = baseResources

	if len(cfg.tfvarsFiles) > 0 {
		for _, env := range cfg.tfvarsFiles {
			vars := cloneValueMap(cfg.variableDefault)
			for key, value := range env.variables {
				vars[key] = value
			}

			locals, err := evaluateLocals(cfg.locals, vars)
			if err != nil {
				return nil, fmt.Errorf("evaluate locals for %s: %w", env.path, err)
			}
			resources, err := expandResources(cfg.resourceBlocks, buildEvalContext(vars, locals))
			if err != nil {
				return nil, fmt.Errorf("expand resources for %s: %w", env.path, err)
			}
			for i := range resources {
				resources[i].Environment = env.name
			}

			analysis.Environments = append(analysis.Environments, EnvironmentAnalysis{
				Name:       env.name,
				TFVarsFile: env.path,
				Resources:  resources,
			})
		}
	}

	if analysis.Backend != nil && p.stateLoader != nil {
		state, err := p.stateLoader.Load(ctx, *analysis.Backend)
		if err == nil {
			analysis.State = state
		}
	}

	return analysis, nil
}

func loadTerraformConfig(root string) (*terraformConfig, error) {
	parser := hclparse.NewParser()
	cfg := &terraformConfig{
		variableDefault: map[string]cty.Value{},
		locals:          map[string]hclsyntax.Expression{},
	}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".terraform" || d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}

		switch {
		case filepath.Ext(path) == ".tf":
			return loadTFFile(parser, cfg, path)
		case strings.HasSuffix(path, ".tfvars"):
			return loadTFVarsFile(parser, cfg, root, path)
		default:
			return nil
		}
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(cfg.tfvarsFiles, func(i, j int) bool { return cfg.tfvarsFiles[i].path < cfg.tfvarsFiles[j].path })
	return cfg, nil
}

func loadTFFile(parser *hclparse.Parser, cfg *terraformConfig, path string) error {
	file, diags := parser.ParseHCLFile(path)
	if diags.HasErrors() {
		return fmt.Errorf("parse %s: %s", path, diags.Error())
	}

	body, ok := file.Body.(*hclsyntax.Body)
	if !ok {
		return fmt.Errorf("unexpected body type for %s", path)
	}

	if cfg.backend == nil {
		if backend := detectS3Backend(body); backend != nil {
			cfg.backend = backend
		}
	}

	for _, block := range body.Blocks {
		switch block.Type {
		case "resource":
			if len(block.Labels) == 2 {
				cfg.resourceBlocks = append(cfg.resourceBlocks, resourceBlock{file: path, block: block})
			}
		case "variable":
			if len(block.Labels) == 1 {
				if attr := block.Body.Attributes["default"]; attr != nil {
					if value, ok := evalLiteral(attr.Expr); ok {
						cfg.variableDefault[block.Labels[0]] = value
					}
				}
			}
		case "locals":
			for name, attr := range block.Body.Attributes {
				cfg.locals[name] = attr.Expr
			}
		}
	}

	return nil
}

func loadTFVarsFile(parser *hclparse.Parser, cfg *terraformConfig, root, path string) error {
	file, diags := parser.ParseHCLFile(path)
	if diags.HasErrors() {
		return fmt.Errorf("parse %s: %s", path, diags.Error())
	}

	body, ok := file.Body.(*hclsyntax.Body)
	if !ok {
		return fmt.Errorf("unexpected tfvars body type for %s", path)
	}

	vars := map[string]cty.Value{}
	for name, attr := range body.Attributes {
		if value, ok := evalLiteral(attr.Expr); ok {
			vars[name] = value
		}
	}

	rel, err := filepath.Rel(root, path)
	if err != nil {
		rel = path
	}
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	cfg.tfvarsFiles = append(cfg.tfvarsFiles, tfvarsFile{
		name:      name,
		path:      rel,
		variables: vars,
	})
	return nil
}

func detectS3Backend(body *hclsyntax.Body) *S3Backend {
	for _, block := range body.Blocks {
		if block.Type != "terraform" {
			continue
		}
		for _, nested := range block.Body.Blocks {
			if nested.Type != "backend" || len(nested.Labels) == 0 || nested.Labels[0] != "s3" {
				continue
			}

			return &S3Backend{
				Bucket:             expressionString(nested.Body.Attributes["bucket"]),
				Key:                expressionString(nested.Body.Attributes["key"]),
				Region:             expressionString(nested.Body.Attributes["region"]),
				WorkspaceKeyPrefix: expressionString(nested.Body.Attributes["workspace_key_prefix"]),
			}
		}
	}
	return nil
}

func expandResources(blocks []resourceBlock, ctx *hcl.EvalContext) ([]Resource, error) {
	var resources []Resource
	for _, item := range blocks {
		expanded, err := expandResourceBlock(item.file, item.block, ctx)
		if err != nil {
			return nil, err
		}
		resources = append(resources, expanded...)
	}
	return resources, nil
}

func expandResourceBlock(path string, block *hclsyntax.Block, parentCtx *hcl.EvalContext) ([]Resource, error) {
	switch {
	case block.Body.Attributes["count"] != nil:
		count, err := evaluateCount(block.Body.Attributes["count"].Expr, parentCtx)
		if err != nil {
			return nil, fmt.Errorf("evaluate count for %s.%s in %s: %w", block.Labels[0], block.Labels[1], filepath.Base(path), err)
		}
		var resources []Resource
		for i := 0; i < count; i++ {
			child := childEvalContext(parentCtx)
			child.Variables["count"] = cty.ObjectVal(map[string]cty.Value{
				"index": cty.NumberIntVal(int64(i)),
			})
			resource, err := materializeResource(path, block, child, strconv.Itoa(i))
			if err != nil {
				return nil, err
			}
			resources = append(resources, resource)
		}
		return resources, nil
	case block.Body.Attributes["for_each"] != nil:
		instances, err := evaluateForEach(block.Body.Attributes["for_each"].Expr, parentCtx)
		if err != nil {
			return nil, fmt.Errorf("evaluate for_each for %s.%s in %s: %w", block.Labels[0], block.Labels[1], filepath.Base(path), err)
		}
		var resources []Resource
		for _, instance := range instances {
			child := childEvalContext(parentCtx)
			child.Variables["each"] = cty.ObjectVal(map[string]cty.Value{
				"key":   cty.StringVal(instance.Key),
				"value": instance.Value,
			})
			resource, err := materializeResource(path, block, child, instance.Key)
			if err != nil {
				return nil, err
			}
			resources = append(resources, resource)
		}
		return resources, nil
	default:
		resource, err := materializeResource(path, block, parentCtx, "")
		if err != nil {
			return nil, err
		}
		return []Resource{resource}, nil
	}
}

type eachInstance struct {
	Key   string
	Value cty.Value
}

func materializeResource(path string, block *hclsyntax.Block, ctx *hcl.EvalContext, instanceKey string) (Resource, error) {
	resource := Resource{
		Type:        block.Labels[0],
		Name:        block.Labels[1],
		File:        filepath.Base(path),
		InstanceKey: instanceKey,
		Attributes:  materializeBody(path, block.Body, ctx),
	}

	return resource, nil
}

func materializeBody(path string, body *hclsyntax.Body, ctx *hcl.EvalContext) map[string]Value {
	attributes := map[string]Value{}

	for name, attr := range body.Attributes {
		if name == "count" || name == "for_each" {
			continue
		}

		value, err := evalExpression(attr.Expr, ctx)
		if err != nil {
			value = literalValue(attr.Expr)
		}
		attributes[name] = Value{
			Raw:     strings.TrimSpace(hclRangeSnippet(attr.Expr.Range(), path)),
			Literal: value,
		}
	}

	groupedBlocks := map[string][]any{}
	for _, block := range body.Blocks {
		literal := materializeNestedBlock(path, block, ctx)
		groupedBlocks[block.Type] = append(groupedBlocks[block.Type], literal)
	}

	for name, items := range groupedBlocks {
		var literal any
		if len(items) == 1 {
			literal = items[0]
		} else {
			literal = items
		}
		attributes[name] = Value{Literal: literal}
	}

	return attributes
}

func materializeNestedBlock(path string, block *hclsyntax.Block, ctx *hcl.EvalContext) any {
	content := map[string]any{}
	if len(block.Labels) > 0 {
		content["labels"] = append([]string(nil), block.Labels...)
	}

	for name, value := range materializeBody(path, block.Body, ctx) {
		content[name] = value.Literal
	}
	return content
}

func buildEvalContext(vars, locals map[string]cty.Value) *hcl.EvalContext {
	ctx := &hcl.EvalContext{
		Variables: map[string]cty.Value{},
		Functions: terraformFunctions(),
	}
	ctx.Variables["var"] = cty.ObjectVal(normalizeValueMap(vars))
	ctx.Variables["local"] = cty.ObjectVal(normalizeValueMap(locals))
	return ctx
}

func childEvalContext(parent *hcl.EvalContext) *hcl.EvalContext {
	child := &hcl.EvalContext{
		Variables: map[string]cty.Value{},
		Functions: parent.Functions,
	}
	for key, value := range parent.Variables {
		child.Variables[key] = value
	}
	return child
}

func terraformFunctions() map[string]function.Function {
	return map[string]function.Function{
		"compact":  compactFunc(),
		"concat":   stdlib.ConcatFunc,
		"contains": stdlib.ContainsFunc,
		"lookup":   lookupFunc(),
		"distinct": stdlib.DistinctFunc,
		"flatten":  stdlib.FlattenFunc,
		"keys":     stdlib.KeysFunc,
		"length":   stdlib.LengthFunc,
		"merge":    stdlib.MergeFunc,
		"tonumber": toNumberFunc(),
		"tostring": toStringFunc(),
		"toset":    toSetFunc(),
		"tolist":   toListFunc(),
		"tomap":    toMapFunc(),
		"values":   stdlib.ValuesFunc,
	}
}

func compactFunc() function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "list", Type: cty.DynamicPseudoType}},
		Type: func(args []cty.Value) (cty.Type, error) {
			value := args[0]
			switch {
			case value.Type().IsTupleType(), value.Type().IsListType(), value.Type().IsSetType():
				return cty.List(cty.String), nil
			default:
				return cty.DynamicPseudoType, nil
			}
		},
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			value := args[0]
			if !(value.Type().IsTupleType() || value.Type().IsListType() || value.Type().IsSetType()) {
				return cty.NilVal, fmt.Errorf("compact requires a list, tuple, or set")
			}

			var values []cty.Value
			it := value.ElementIterator()
			for it.Next() {
				_, item := it.Element()
				if !item.IsKnown() || item.IsNull() || !item.Type().Equals(cty.String) {
					return cty.NilVal, fmt.Errorf("compact requires string elements")
				}
				if item.AsString() == "" {
					continue
				}
				values = append(values, item)
			}
			if len(values) == 0 {
				return cty.ListValEmpty(cty.String), nil
			}
			return cty.ListVal(values), nil
		},
	})
}

func lookupFunc() function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{
			{Name: "inputMap", Type: cty.DynamicPseudoType},
			{Name: "key", Type: cty.String},
		},
		VarParam: &function.Parameter{Name: "default", Type: cty.DynamicPseudoType},
		Type: func(args []cty.Value) (cty.Type, error) {
			if len(args) == 3 && args[2].Type() != cty.DynamicPseudoType {
				return args[2].Type(), nil
			}

			value := args[0]
			switch {
			case value.Type().IsMapType():
				return value.Type().ElementType(), nil
			case value.Type().IsObjectType():
				if value.IsNull() || !value.IsKnown() {
					return cty.DynamicPseudoType, nil
				}
				if attrType, ok := value.Type().AttributeTypes()[args[1].AsString()]; ok {
					return attrType, nil
				}
			}
			return cty.DynamicPseudoType, nil
		},
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			value := args[0]
			key := args[1].AsString()

			switch {
			case value.Type().IsMapType():
				if !value.IsKnown() || value.IsNull() {
					break
				}
				if item := value.Index(cty.StringVal(key)); item.IsKnown() && !item.IsNull() {
					return item, nil
				}
			case value.Type().IsObjectType():
				if !value.IsKnown() || value.IsNull() {
					break
				}
				if item := value.GetAttr(key); item.IsKnown() && !item.IsNull() {
					return item, nil
				}
			}

			if len(args) == 3 {
				return args[2], nil
			}
			return cty.NilVal, fmt.Errorf("lookup failed to find key %q", key)
		},
	})
}

func toSetFunc() function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "value", Type: cty.DynamicPseudoType}},
		Type: func(args []cty.Value) (cty.Type, error) {
			value := args[0]
			if value.Type().IsSetType() {
				return value.Type(), nil
			}
			switch {
			case value.Type().IsTupleType(), value.Type().IsListType(), value.Type().IsSetType():
				return cty.Set(cty.DynamicPseudoType), nil
			default:
				return cty.DynamicPseudoType, nil
			}
		},
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			value := args[0]
			if value.Type().IsSetType() {
				return value, nil
			}
			if !(value.Type().IsTupleType() || value.Type().IsListType() || value.Type().IsSetType()) {
				return cty.NilVal, fmt.Errorf("toset requires a list, tuple, or set")
			}

			var values []cty.Value
			it := value.ElementIterator()
			for it.Next() {
				_, item := it.Element()
				values = append(values, item)
			}
			if len(values) == 0 {
				return cty.SetValEmpty(cty.DynamicPseudoType), nil
			}
			return cty.SetVal(values), nil
		},
	})
}

func toNumberFunc() function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "value", Type: cty.DynamicPseudoType}},
		Type: func(args []cty.Value) (cty.Type, error) {
			return cty.Number, nil
		},
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			value := args[0]
			switch {
			case value.Type().Equals(cty.Number):
				return value, nil
			case value.Type().Equals(cty.String):
				f, err := strconv.ParseFloat(value.AsString(), 64)
				if err != nil {
					return cty.NilVal, fmt.Errorf("tonumber requires a numeric string: %w", err)
				}
				return cty.NumberFloatVal(f), nil
			default:
				return cty.NilVal, fmt.Errorf("tonumber requires a string or number")
			}
		},
	})
}

func toStringFunc() function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "value", Type: cty.DynamicPseudoType}},
		Type: func(args []cty.Value) (cty.Type, error) {
			return cty.String, nil
		},
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			value := args[0]
			if !value.IsKnown() || value.IsNull() {
				return cty.NilVal, fmt.Errorf("tostring requires a known, non-null value")
			}
			return cty.StringVal(ctyValueString(value)), nil
		},
	})
}

func toListFunc() function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "value", Type: cty.DynamicPseudoType}},
		Type: func(args []cty.Value) (cty.Type, error) {
			value := args[0]
			if value.Type().IsListType() {
				return value.Type(), nil
			}
			switch {
			case value.Type().IsTupleType(), value.Type().IsListType(), value.Type().IsSetType():
				return cty.List(cty.DynamicPseudoType), nil
			default:
				return cty.DynamicPseudoType, nil
			}
		},
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			value := args[0]
			if value.Type().IsListType() {
				return value, nil
			}
			if !(value.Type().IsTupleType() || value.Type().IsListType() || value.Type().IsSetType()) {
				return cty.NilVal, fmt.Errorf("tolist requires a list, tuple, or set")
			}

			var values []cty.Value
			it := value.ElementIterator()
			for it.Next() {
				_, item := it.Element()
				values = append(values, item)
			}
			return cty.ListVal(values), nil
		},
	})
}

func toMapFunc() function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "value", Type: cty.DynamicPseudoType}},
		Type: func(args []cty.Value) (cty.Type, error) {
			value := args[0]
			if value.Type().IsMapType() {
				return value.Type(), nil
			}
			if value.Type().IsObjectType() {
				return cty.Map(cty.DynamicPseudoType), nil
			}
			return cty.DynamicPseudoType, nil
		},
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			value := args[0]
			if value.Type().IsMapType() {
				return value, nil
			}
			if !value.Type().IsObjectType() {
				return cty.NilVal, fmt.Errorf("tomap requires an object or map")
			}
			return cty.MapVal(value.AsValueMap()), nil
		},
	})
}

func normalizeValueMap(values map[string]cty.Value) map[string]cty.Value {
	if len(values) == 0 {
		return map[string]cty.Value{}
	}
	out := map[string]cty.Value{}
	for key, value := range values {
		if !value.IsNull() {
			out[key] = value
		}
	}
	return out
}

func cloneValueMap(values map[string]cty.Value) map[string]cty.Value {
	out := map[string]cty.Value{}
	for key, value := range values {
		out[key] = value
	}
	return out
}

func evaluateLocals(localExprs map[string]hclsyntax.Expression, vars map[string]cty.Value) (map[string]cty.Value, error) {
	resolved := map[string]cty.Value{}
	pending := map[string]hclsyntax.Expression{}
	lastErrors := map[string]string{}
	for key, expr := range localExprs {
		pending[key] = expr
	}

	for len(pending) > 0 {
		progress := false
		ctx := buildEvalContext(vars, resolved)
		for key, expr := range pending {
			value, diags := expr.Value(ctx)
			if diags.HasErrors() {
				lastErrors[key] = strings.TrimSpace(diags.Error())
				continue
			}
			resolved[key] = value
			delete(pending, key)
			delete(lastErrors, key)
			progress = true
		}
		if !progress {
			break
		}
	}

	if len(pending) == 0 {
		return resolved, nil
	}

	names := make([]string, 0, len(pending))
	for key := range pending {
		names = append(names, key)
	}
	sort.Strings(names)

	var details []string
	for _, key := range names {
		detail := lastErrors[key]
		if detail == "" {
			detail = "unknown evaluation error"
		}
		details = append(details, fmt.Sprintf("%s: %s", key, detail))
	}

	return nil, fmt.Errorf("could not resolve locals: %s", strings.Join(details, "; "))
}

func evaluateCount(expr hclsyntax.Expression, ctx *hcl.EvalContext) (int, error) {
	value, diags := expr.Value(ctx)
	if diags.HasErrors() {
		return 0, fmt.Errorf("%s", diags.Error())
	}
	if !value.Type().Equals(cty.Number) {
		return 0, fmt.Errorf("count did not evaluate to a number")
	}
	bf := value.AsBigFloat()
	count, _ := bf.Int64()
	if count < 0 {
		return 0, fmt.Errorf("count must not be negative")
	}
	return int(count), nil
}

func evaluateForEach(expr hclsyntax.Expression, ctx *hcl.EvalContext) ([]eachInstance, error) {
	value, diags := expr.Value(ctx)
	if diags.HasErrors() {
		return nil, fmt.Errorf("%s", diags.Error())
	}

	var instances []eachInstance
	switch {
	case value.Type().IsMapType() || value.Type().IsObjectType():
		valueMap := value.AsValueMap()
		keys := make([]string, 0, len(valueMap))
		for key := range valueMap {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			instances = append(instances, eachInstance{Key: key, Value: valueMap[key]})
		}
	case value.Type().IsSetType():
		it := value.ElementIterator()
		for it.Next() {
			_, v := it.Element()
			key := ctyValueString(v)
			instances = append(instances, eachInstance{Key: key, Value: v})
		}
		sort.Slice(instances, func(i, j int) bool { return instances[i].Key < instances[j].Key })
	case value.Type().IsTupleType() || value.Type().IsListType():
		it := value.ElementIterator()
		for it.Next() {
			_, v := it.Element()
			key := ctyValueString(v)
			instances = append(instances, eachInstance{Key: key, Value: v})
		}
	default:
		return nil, fmt.Errorf("for_each must evaluate to a collection")
	}

	return instances, nil
}

func expressionString(attr *hclsyntax.Attribute) string {
	if attr == nil {
		return ""
	}
	if value, err := evalExpression(attr.Expr, nil); err == nil {
		return fmt.Sprint(value)
	}
	if value := literalValue(attr.Expr); value != nil {
		return fmt.Sprint(value)
	}
	return strings.TrimSpace(hclRangeSnippet(attr.Expr.Range(), pathFromRange(attr.Expr.Range())))
}

func evalLiteral(expr hclsyntax.Expression) (cty.Value, bool) {
	value, diags := expr.Value(nil)
	if diags.HasErrors() {
		return cty.NilVal, false
	}
	return value, true
}

func evalExpression(expr hclsyntax.Expression, ctx *hcl.EvalContext) (any, error) {
	value, diags := expr.Value(ctx)
	if diags.HasErrors() {
		return nil, fmt.Errorf("%s", diags.Error())
	}
	return ctyToGo(value), nil
}

func literalValue(expr hclsyntax.Expression) any {
	value, ok := evalLiteral(expr)
	if !ok {
		return nil
	}
	return ctyToGo(value)
}

func ctyToGo(value cty.Value) any {
	if !value.IsKnown() || value.IsNull() {
		return nil
	}
	switch {
	case value.Type().Equals(cty.String):
		return value.AsString()
	case value.Type().Equals(cty.Bool):
		return value.True()
	case value.Type().Equals(cty.Number):
		f, _ := value.AsBigFloat().Float64()
		return f
	case value.Type().IsMapType() || value.Type().IsObjectType():
		out := map[string]any{}
		for key, item := range value.AsValueMap() {
			out[key] = ctyToGo(item)
		}
		return out
	case value.Type().IsTupleType() || value.Type().IsListType() || value.Type().IsSetType():
		var out []any
		it := value.ElementIterator()
		for it.Next() {
			_, item := it.Element()
			out = append(out, ctyToGo(item))
		}
		return out
	default:
		return ctyValueString(value)
	}
}

func ctyValueString(value cty.Value) string {
	goValue := ctyToGo(value)
	switch v := goValue.(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		if v {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprint(goValue)
	}
}

func pathFromRange(r hcl.Range) string {
	if r.Filename != "" {
		return r.Filename
	}
	return ""
}

func hclRangeSnippet(r hcl.Range, path string) string {
	content, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	start := r.Start.Byte
	end := r.End.Byte
	if start < 0 || end > len(content) || start >= end {
		return ""
	}
	return string(content[start:end])
}
