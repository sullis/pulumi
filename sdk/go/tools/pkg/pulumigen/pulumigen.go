//nolint: lll, goconst
package pulumigen

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/token"
	"io"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/tools/go/packages"
)

type typeOptions struct {
	name      string
	suffix    string
	args      bool
	noInputs  bool
	noOutputs bool
	noPtr     bool
	noArray   bool
	noMap     bool
}

type structType struct {
	structName string
	opts       typeOptions
	doc        *ast.CommentGroup
	fields     []*ast.Field
}

func (t structType) name() string {
	return t.opts.name
}

func (t structType) suffix() string {
	return t.opts.suffix
}

func title(s string) string {
	if s == "" {
		return ""
	}
	runes := []rune(s)
	return string(append([]rune{unicode.ToUpper(runes[0])}, runes[1:]...))
}

func camel(s string) string {
	if s == "" {
		return ""
	}
	runes := []rune(s)
	res := make([]rune, 0, len(runes))
	for i, r := range runes {
		if unicode.IsLower(r) {
			res = append(res, runes[i:]...)
			break
		}
		res = append(res, unicode.ToLower(r))
	}
	return string(res)
}

func skipParens(x ast.Expr) ast.Expr {
	for paren, isParen := x.(*ast.ParenExpr); isParen; paren, isParen = x.(*ast.ParenExpr) {
		x = paren.X
	}
	return x
}

func hasPulumiComment(doc *ast.CommentGroup) (typeOptions, bool) {
	if doc == nil {
		return typeOptions{}, false
	}

	lastComment := doc.List[len(doc.List)-1].Text
	if !strings.HasPrefix(lastComment, "// +pulumi") {
		return typeOptions{}, false
	}

	var opts typeOptions
	for _, opt := range strings.Split(lastComment[len("// +pulumi"):], " ") {
		parts := strings.SplitN(opt, ":", 2)
		switch parts[0] {
		case "name":
			if len(parts) == 2 {
				opts.name = parts[1]
			}
		case "suffix":
			if len(parts) == 2 {
				opts.suffix = parts[1]
			}
		case "args":
			opts.args = true
			opts.noOutputs = true
			opts.noPtr = true
			opts.noArray = true
			opts.noMap = true
		case "-inputs":
			opts.noInputs = true
		case "-outputs":
			opts.noOutputs = true
			opts.noPtr = true
			opts.noArray = true
			opts.noMap = true
		case "-ptr":
			opts.noPtr = true
		case "-array":
			opts.noArray = true
		case "-map":
			opts.noMap = true
		}
	}

	// Trim the pulumi comment off the group.
	doc.List = doc.List[:len(doc.List)-1]
	return opts, true
}

func gatherTypes(files []*ast.File) ([]structType, []string) {
	var types []structType
	var imports []string

	importedNames := map[string]bool{}

	needsContext, needsReflect, needsPulumi := false, false, false
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			decl, ok := n.(*ast.GenDecl)
			if !ok || decl.Tok != token.TYPE {
				// We only need to examine type declarations.
				return true
			}

			declOptions, hasDeclComment := hasPulumiComment(decl.Doc)
			for _, spec := range decl.Specs {
				typeSpec := spec.(*ast.TypeSpec)

				// Look for a "// +pulumi" comment on the type.
				specOptions, hasSpecComment := hasPulumiComment(typeSpec.Doc)
				if !hasDeclComment && !hasSpecComment {
					continue
				}

				structT, ok := skipParens(typeSpec.Type).(*ast.StructType)
				if !ok {
					continue
				}

				ast.Inspect(structT, func(n ast.Node) bool {
					selector, ok := n.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					importedNames[selector.X.(*ast.Ident).Name] = true
					return false
				})

				doc := typeSpec.Doc
				if doc == nil {
					doc = decl.Doc
				}

				opts := specOptions
				if !hasSpecComment {
					opts = declOptions
				}

				if opts.name == "" {
					opts.name = typeSpec.Name.Name
				}
				if opts.suffix == "" {
					opts.suffix = "Inputs"
				}

				if !opts.noInputs || !opts.noOutputs {
					needsReflect, needsPulumi = true, len(structT.Fields.List) > 0
				}
				if !opts.noOutputs {
					needsContext = true
				}

				types = append(types, structType{
					structName: typeSpec.Name.Name,
					opts:       opts,
					doc:        doc,
					fields:     structT.Fields.List,
				})
			}
			return false
		})
	}

	sort.Slice(types, func(i, j int) bool {
		return types[i].name() < types[j].name()
	})

	if needsContext {
		imports = append(imports, `"context"`)
	}
	if needsReflect {
		imports = append(imports, `"reflect"`)
	}
	if needsPulumi {
		if len(imports) == 0 {
			imports = append(imports, "")
		}
		imports = append(imports, `"github.com/pulumi/pulumi/sdk/go/pulumi"`)
	}

	if len(types) > 0 {
		importSet := map[string]bool{}
		for _, f := range files {
			for _, i := range f.Imports {
				if i.Name != nil && importedNames[i.Name.Name] && !importSet[i.Path.Value] {
					imports = append(imports, fmt.Sprintf(`%s %s`, i.Name.Name, i.Path.Value))
					importSet[i.Path.Value] = true
				}
			}
		}
	}

	return types, imports
}

func inputTypeName(t ast.Expr) string {
	switch t := skipParens(t).(type) {
	case *ast.ArrayType:
		en := inputTypeName(t.Elt)
		return strings.TrimSuffix(en, "Input") + "ArrayInput"
	case *ast.Ident:
		switch t.Name {
		case "bool", "byte", "float32", "float64",
			"int", "int8", "int16", "int32", "int64", "string",
			"uint", "uint8", "uint16", "uint32", "uint64":
			return "pulumi." + title(t.Name) + "Input"
		case "complex64", "complex128", "error", "rune", "uintptr":
			return "pulumi.Input"
		}
		return t.Name + "Input"
	case *ast.MapType:
		vn := inputTypeName(t.Value)
		return strings.TrimSuffix(vn, "Input") + "MapInput"
	case *ast.SelectorExpr:
		return t.X.(*ast.Ident).Name + "." + t.Sel.Name + "Input"
	case *ast.StarExpr:
		xn := inputTypeName(t.X)
		return strings.TrimSuffix(xn, "Input") + "PtrInput"
	default:
		return "pulumi.Input"
	}
}

func outputTypeName(t ast.Expr) (string, string) {
	switch t := skipParens(t).(type) {
	case *ast.ArrayType:
		en, ee := outputTypeName(t.Elt)
		if en == "pulumi.AnyOutput" {
			return "pulumi.ArrayOutput", "[]interface{}"
		}
		return strings.TrimSuffix(en, "Output") + "ArrayOutput", "[]" + ee
	case *ast.Ident:
		switch t.Name {
		case "bool", "byte", "float32", "float64",
			"int", "int8", "int16", "int32", "int64", "string",
			"uint", "uint8", "uint16", "uint32", "uint64":
			return "pulumi." + title(t.Name) + "Output", t.Name
		case "complex64", "complex128", "error", "rune", "uintptr":
			return "pulumi.AnyOutput", "interface{}"
		}
		return t.Name + "Output", t.Name
	case *ast.MapType:
		vn, ve := outputTypeName(t.Value)
		if vn == "pulumi.AnyOutput" {
			return "pulumi.MapOutput", "map[string]interface{}"
		}
		return strings.TrimSuffix(vn, "Output") + "MapOutput", "map[string]" + ve
	case *ast.SelectorExpr:
		n := t.X.(*ast.Ident).Name + "." + t.Sel.Name
		return n + "Output", n
	case *ast.StarExpr:
		xn, xe := outputTypeName(t.X)
		if xn == "pulumi.AnyOutput" {
			return "pulumi.AnyOutput", "interface{}"
		}
		return strings.TrimSuffix(xn, "Output") + "PtrOutput", "*" + xe
	default:
		return "pulumi.AnyOutput", "interface{}"
	}
}

func printComments(w io.Writer, comments *ast.CommentGroup, indent bool) {
	if comments != nil {
		for _, c := range comments.List {
			lines := strings.Split(c.Text, "\n")
			for _, l := range lines {
				if indent {
					fmt.Fprintf(w, "\t")
				}
				fmt.Fprintf(w, "%s\n", l)
			}
		}
	}
}

func genInputInterface(w io.Writer, name string) {
	fmt.Fprintf(w, "type %sInput interface {\n", name)
	fmt.Fprintf(w, "\tpulumi.Input\n\n")
	fmt.Fprintf(w, "\tTo%sOutput() %sOutput\n", title(name), name)
	fmt.Fprintf(w, "\tTo%sOutputWithContext(context.Context) %sOutput\n", title(name), name)
	fmt.Fprintf(w, "}\n\n")
}

func genInputMethods(w io.Writer, name, receiverType, elementType string, noOutputs, ptrMethods bool) {
	fmt.Fprintf(w, "func (%s) ElementType() reflect.Type {\n", receiverType)
	fmt.Fprintf(w, "\treturn reflect.TypeOf((*%s)(nil)).Elem()\n", elementType)
	fmt.Fprintf(w, "}\n\n")

	if !noOutputs {
		fmt.Fprintf(w, "func (i %s) To%sOutput() %sOutput {\n", receiverType, title(name), name)
		fmt.Fprintf(w, "\treturn i.To%sOutputWithContext(context.Background())\n", title(name))
		fmt.Fprintf(w, "}\n\n")

		fmt.Fprintf(w, "func (i %s) To%sOutputWithContext(ctx context.Context) %sOutput {\n", receiverType, title(name), name)
		fmt.Fprintf(w, "\treturn pulumi.ToOutputWithContext(ctx, i).(%sOutput)\n", name)
		fmt.Fprintf(w, "}\n\n")

		if ptrMethods {
			fmt.Fprintf(w, "func (i %s) To%sPtrOutput() %sPtrOutput {\n", receiverType, title(name), name)
			fmt.Fprintf(w, "\treturn i.To%sPtrOutputWithContext(context.Background())\n", title(name))
			fmt.Fprintf(w, "}\n\n")

			fmt.Fprintf(w, "func (i %s) To%sPtrOutputWithContext(ctx context.Context) %sPtrOutput {\n", receiverType, title(name), name)
			fmt.Fprintf(w, "\treturn pulumi.ToOutputWithContext(ctx, i).(%[1]sOutput).To%[1]sPtrOutputWithContext(ctx)\n", name)
			fmt.Fprintf(w, "}\n\n")
		}
	}
}

func genInputTypes(w io.Writer, t structType) {
	// Generate the plain inputs.
	if !t.opts.noOutputs {
		genInputInterface(w, t.name())
	}

	printComments(w, t.doc, false)
	fmt.Fprintf(w, "type %s%s struct {\n", t.name(), t.suffix())
	for _, f := range t.fields {
		printComments(w, f.Doc, true)
		fmt.Fprintf(w, "\t")
		for i, n := range f.Names {
			if i > 0 {
				fmt.Fprintf(w, ", ")
			}
			fmt.Fprintf(w, "%s", n.Name)
		}
		fmt.Fprintf(w, " %s", inputTypeName(f.Type))
		if f.Comment != nil {
			fmt.Fprintf(w, " ")
			printComments(w, f.Comment, false)
		} else {
			fmt.Fprintf(w, "\n")
		}
	}
	fmt.Fprintf(w, "}\n\n")

	receiverType := t.name() + t.suffix()
	if t.opts.args {
		receiverType = "*" + receiverType
	}
	genInputMethods(w, t.name(), receiverType, t.structName, t.opts.noOutputs, !t.opts.noPtr)

	// Generate the pointer input.
	if !t.opts.noPtr {
		genInputInterface(w, t.name()+"Ptr")

		ptrTypeName := camel(t.name()) + "PtrType"

		fmt.Fprintf(w, "type %s %sInputs\n\n", ptrTypeName, t.name())

		fmt.Fprintf(w, "func %[1]sPtr(v *%[1]sInputs) %[1]sPtrInput {", t.name())
		fmt.Fprintf(w, "\treturn (*%s)(v)\n", ptrTypeName)
		fmt.Fprintf(w, "}\n\n")

		genInputMethods(w, t.name()+"Ptr", "*"+ptrTypeName, "*"+t.name(), false, false)
	}

	// Generate the array input.
	if !t.opts.noArray {
		genInputInterface(w, t.name()+"Array")

		fmt.Fprintf(w, "type %[1]sArray []%[1]sInput\n\n", t.name())

		genInputMethods(w, t.name()+"Array", t.name()+"Array", "[]"+t.name(), false, false)
	}

	// Generate the map input.
	if !t.opts.noMap {
		genInputInterface(w, t.name()+"Map")

		fmt.Fprintf(w, "type %[1]sMap map[string]%[1]sInput\n\n", t.name())

		genInputMethods(w, t.name()+"Map", t.name()+"Map", "map[string]"+t.name(), false, false)
	}
}

func genOutputMethods(w io.Writer, name, elementType string) {
	fmt.Fprintf(w, "func (%sOutput) ElementType() reflect.Type {\n", name)
	fmt.Fprintf(w, "\treturn reflect.TypeOf((*%s)(nil)).Elem()\n", elementType)
	fmt.Fprintf(w, "}\n\n")

	fmt.Fprintf(w, "func (o %[1]sOutput) To%[2]sOutput() %[1]sOutput {\n", name, title(name))
	fmt.Fprintf(w, "\treturn o\n")
	fmt.Fprintf(w, "}\n\n")

	fmt.Fprintf(w, "func (o %[1]sOutput) To%[2]sOutputWithContext(ctx context.Context) %[1]sOutput {\n", name, title(name))
	fmt.Fprintf(w, "\treturn o\n")
	fmt.Fprintf(w, "}\n\n")
}

func genOutputTypes(w io.Writer, t structType) {
	printComments(w, t.doc, false)
	fmt.Fprintf(w, "type %sOutput struct { *pulumi.OutputState }\n\n", t.name())

	genOutputMethods(w, t.name(), t.name())

	if !t.opts.noPtr {
		fmt.Fprintf(w, "func (o %[1]sOutput) To%[2]sPtrOutput() %[1]sPtrOutput {\n", t.name(), title(t.name()))
		fmt.Fprintf(w, "\treturn o.To%sPtrOutputWithContext(context.Background())\n", title(t.name()))
		fmt.Fprintf(w, "}\n\n")

		fmt.Fprintf(w, "func (o %[1]sOutput) To%[2]sPtrOutputWithContext(ctx context.Context) %[1]sPtrOutput {\n", t.name(), title(t.name()))
		fmt.Fprintf(w, "\treturn o.ApplyT(func(v %[1]s) *%[1]s {\n", t.name())
		fmt.Fprintf(w, "\t\treturn &v\n")
		fmt.Fprintf(w, "\t}).(%sPtrOutput)\n", t.name())
		fmt.Fprintf(w, "}\n")
	}

	for _, f := range t.fields {
		printComments(w, f.Doc, false)
		printComments(w, f.Comment, false)
		outputType, applyType := outputTypeName(f.Type)
		for _, n := range f.Names {
			fmt.Fprintf(w, "func (o %sOutput) %s() %s {\n", t.name(), n.Name, outputType)
			fmt.Fprintf(w, "\treturn o.ApplyT(func (v %s) %s { return v.%s }).(%s)\n", t.name(), applyType, n.Name, outputType)
			fmt.Fprintf(w, "}\n\n")
		}
	}

	if !t.opts.noPtr {
		fmt.Fprintf(w, "type %sPtrOutput struct { *pulumi.OutputState}\n\n", t.name())

		genOutputMethods(w, t.name()+"Ptr", "*"+t.name())

		fmt.Fprintf(w, "func (o %[1]sPtrOutput) Elem() %[1]sOutput {\n", t.name())
		fmt.Fprintf(w, "\treturn o.ApplyT(func (v *%[1]s) %[1]s { return *v }).(%[1]sOutput)\n", t.name())
		fmt.Fprintf(w, "}\n\n")

		for _, f := range t.fields {
			outputType, applyType := outputTypeName(f.Type)
			for _, n := range f.Names {
				fmt.Fprintf(w, "func (o %sPtrOutput) %s() %s {\n", t.name(), n.Name, outputType)
				fmt.Fprintf(w, "\treturn o.ApplyT(func (v *%s) %s { return v.%s }).(%s)\n", t.name(), applyType, n.Name, outputType)
				fmt.Fprintf(w, "}\n\n")
			}
		}
	}

	if !t.opts.noArray {
		fmt.Fprintf(w, "type %sArrayOutput struct { *pulumi.OutputState}\n\n", t.name())

		genOutputMethods(w, t.name()+"Array", "[]"+t.name())

		fmt.Fprintf(w, "func (o %[1]sArrayOutput) Index(i pulumi.IntInput) %[1]sOutput {\n", t.name())
		fmt.Fprintf(w, "\treturn pulumi.All(o, i).ApplyT(func (vs []interface{}) %s {\n", t.name())
		fmt.Fprintf(w, "\t\treturn vs[0].([]%s)[vs[1].(int)]\n", t.name())
		fmt.Fprintf(w, "\t}).(%sOutput)\n", t.name())
		fmt.Fprintf(w, "}\n\n")
	}

	if !t.opts.noMap {
		fmt.Fprintf(w, "type %sMapOutput struct { *pulumi.OutputState}\n\n", t.name())

		genOutputMethods(w, t.name()+"Map", "map[string]"+t.name())

		fmt.Fprintf(w, "func (o %[1]sMapOutput) MapIndex(k pulumi.StringInput) %[1]sOutput {\n", t.name())
		fmt.Fprintf(w, "\treturn pulumi.All(o, k).ApplyT(func (vs []interface{}) %s {\n", t.name())
		fmt.Fprintf(w, "\t\treturn vs[0].(map[string]%s)[vs[1].(string)]\n", t.name())
		fmt.Fprintf(w, "\t}).(%sOutput)\n", t.name())
		fmt.Fprintf(w, "}\n\n")
	}
}

func genPulumiTypes(w io.Writer, t structType) {
	// Generate the input types.
	if !t.opts.noInputs {
		genInputTypes(w, t)
	}

	// Generate the output types.
	if !t.opts.noOutputs {
		genOutputTypes(w, t)
	}
}

func genInitFn(w io.Writer, types []structType) {
	needsInit := false
	for _, t := range types {
		if !t.opts.noOutputs {
			needsInit = true
		}
	}
	if !needsInit {
		return
	}

	fmt.Fprintf(w, "func init() {\n")
	for _, t := range types {
		if t.opts.noOutputs {
			continue
		}

		fmt.Fprintf(w, "\tpulumi.RegisterOutputType(%sOutput{})\n", t.name())
		if !t.opts.noPtr {
			fmt.Fprintf(w, "\tpulumi.RegisterOutputType(%sPtrOutput{})\n", t.name())
		}
		if !t.opts.noArray {
			fmt.Fprintf(w, "\tpulumi.RegisterOutputType(%sArrayOutput{})\n", t.name())
		}
		if !t.opts.noMap {
			fmt.Fprintf(w, "\tpulumi.RegisterOutputType(%sMapOutput{})\n", t.name())
		}
	}
	fmt.Fprintf(w, "}\n")
}

func genHeader(w io.Writer, packageName string, imports []string) {
	fmt.Fprintf(w, "// Code generated by \"pulumigen\"; DO NOT EDIT.\n\n")

	fmt.Fprintf(w, "package %s\n\n", packageName)

	if len(imports) > 0 {
		fmt.Fprintf(w, "import (\n")
		for _, i := range imports {
			if i == "" {
				fmt.Fprintf(w, "\n")
			} else {
				fmt.Fprintf(w, "\t%s\n", i)
			}
		}
		fmt.Fprintf(w, ")\n\n")
	}
}

func generatePulumiTypes(w io.Writer, files []*ast.File, packageName string) error {
	// Buffer the output so we can format it.
	var buffer bytes.Buffer

	// Gather the types to use as the basis for codegen.
	types, imports := gatherTypes(files)

	// Print the header.
	genHeader(&buffer, packageName, imports)

	for _, t := range types {
		genPulumiTypes(&buffer, t)
	}

	// Generate an appropriate init function to register the output types.
	genInitFn(&buffer, types)

	// Format the code.
	formatted, err := format.Source(buffer.Bytes())
	if err != nil {
		return err
	}

	_, err = w.Write(formatted)
	return err
}

func GeneratePulumiTypesFromPackage(w io.Writer, pkg *packages.Package) error {
	return generatePulumiTypes(w, pkg.Syntax, pkg.Name)
}

func GeneratePulumiTypes(w io.Writer, pkg *ast.Package) error {
	var files []*ast.File
	for _, f := range pkg.Files {
		files = append(files, f)
	}

	return generatePulumiTypes(w, files, pkg.Name)
}
