//nolint: lll, goconst
package main

import (
	"log"
	"os"

	"github.com/pulumi/pulumi/sdk/go/tools/pkg/pulumigen"
	"golang.org/x/tools/go/packages"
)

func main() {
	const loadFiles = packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles
	const loadImports = loadFiles | packages.NeedImports | packages.NeedDeps
	const loadTypes = loadImports | packages.NeedTypes | packages.NeedTypesSizes
	const loadSyntax = loadTypes | packages.NeedSyntax | packages.NeedTypesInfo

	packages, err := packages.Load(&packages.Config{
		Mode: loadSyntax,
	}, ".")
	if err != nil {
		log.Fatalf("loading packages: %v", err)
	}

	if err := pulumigen.GeneratePulumiTypesFromPackage(os.Stdout, packages[0]); err != nil {
		log.Fatalf("generating types: %v", err)
	}
}
