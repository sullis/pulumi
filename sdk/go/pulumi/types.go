// Copyright 2016-2018, Pulumi Corporation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// nolint: lll, interfacer
package pulumi

import (
	"context"
	"reflect"
	"strings"
	"sync"

	"github.com/pkg/errors"
	"github.com/pulumi/pulumi/pkg/util/contract"
)

// Output helps encode the relationship between resources in a Pulumi application. Specifically an output property
// holds onto a value and the resource it came from. An output value can then be provided when constructing new
// resources, allowing that new resource to know both the value as well as the resource the value came from.  This
// allows for a precise "dependency graph" to be created, which properly tracks the relationship between resources.
type Output interface {
	ElementType() reflect.Type

	Apply(applier func(interface{}) (interface{}, error)) AnyOutput
	ApplyWithContext(ctx context.Context, applier func(context.Context, interface{}) (interface{}, error)) AnyOutput
	ApplyT(applier interface{}) Output
	ApplyTWithContext(ctx context.Context, applier interface{}) Output

	getState() *OutputState
	dependencies() []Resource
	fulfill(value interface{}, known bool, err error)
	resolve(value interface{}, known bool)
	reject(err error)
	await(ctx context.Context) (interface{}, bool, error)
}

var outputType = reflect.TypeOf((*Output)(nil)).Elem()

var concreteTypeToOutputType sync.Map // map[reflect.Type]reflect.Type

// RegisterOutputType registers an Output type with the Pulumi runtime. If a value of this type's concrete type is
// returned by an Apply, the Apply will return the specific Output type.
func RegisterOutputType(output Output) {
	elementType := output.ElementType()
	existing, hasExisting := concreteTypeToOutputType.LoadOrStore(elementType, reflect.TypeOf(output))
	if hasExisting {
		panic(errors.Errorf("an output type for %v is already registered: %v", elementType, existing))
	}
}

const (
	outputPending = iota
	outputResolved
	outputRejected
)

// OutputState holds the internal details of an Output and implements the Apply and ApplyWithContext methods.
type OutputState struct {
	mutex sync.Mutex
	cond  *sync.Cond

	state uint32 // one of output{Pending,Resolved,Rejected}

	value interface{} // the value of this output if it is resolved.
	err   error       // the error associated with this output if it is rejected.
	known bool        // true if this output's value is known.

	element reflect.Type // the element type of this output.
	deps    []Resource   // the dependencies associated with this output property.
}

func (o *OutputState) elementType() reflect.Type {
	if o == nil {
		return anyType
	}
	return o.element
}

func (o *OutputState) dependencies() []Resource {
	if o == nil {
		return nil
	}
	return o.deps
}

func (o *OutputState) fulfill(value interface{}, known bool, err error) {
	if o == nil {
		return
	}

	o.mutex.Lock()
	defer func() {
		o.mutex.Unlock()
		o.cond.Broadcast()
	}()

	if o.state != outputPending {
		return
	}

	if err != nil {
		o.state, o.err, o.known = outputRejected, err, true
	} else {
		o.state, o.value, o.known = outputResolved, value, known
	}
}

func (o *OutputState) resolve(value interface{}, known bool) {
	o.fulfill(value, known, nil)
}

func (o *OutputState) reject(err error) {
	o.fulfill(nil, true, err)
}

func (o *OutputState) await(ctx context.Context) (interface{}, bool, error) {
	for {
		if o == nil {
			// If the state is nil, treat its value as resolved and unknown.
			return nil, false, nil
		}

		o.mutex.Lock()
		for o.state == outputPending {
			if ctx.Err() != nil {
				return nil, true, ctx.Err()
			}
			o.cond.Wait()
		}
		o.mutex.Unlock()

		if !o.known || o.err != nil {
			return nil, o.known, o.err
		}

		ov, ok := o.value.(Output)
		if !ok {
			return o.value, true, nil
		}
		o = ov.getState()
	}
}

func (o *OutputState) getState() *OutputState {
	return o
}

func newOutputState(elementType reflect.Type, deps ...Resource) *OutputState {
	out := &OutputState{
		element: elementType,
		deps:    deps,
	}
	out.cond = sync.NewCond(&out.mutex)
	return out
}

var outputStateType = reflect.TypeOf((*OutputState)(nil))
var outputTypeToOutputState sync.Map // map[reflect.Type]func(Output, ...Resource)

func newOutput(typ reflect.Type, deps ...Resource) Output {
	contract.Assert(typ.Implements(outputType))

	outputFieldV, ok := outputTypeToOutputState.Load(typ)
	if !ok {
		outputField := -1
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			if f.Anonymous && f.Type == outputStateType {
				outputField = i
				break
			}
		}
		contract.Assert(outputField != -1)
		outputTypeToOutputState.Store(typ, outputField)
		outputFieldV = outputField
	}

	output := reflect.New(typ).Elem()
	state := newOutputState(output.Interface().(Output).ElementType(), deps...)
	output.Field(outputFieldV.(int)).Set(reflect.ValueOf(state))
	return output.Interface().(Output)
}

// NewOutput returns an output value that can be used to rendezvous with the production of a value or error.  The
// function returns the output itself, plus two functions: one for resolving a value, and another for rejecting with an
// error; exactly one function must be called. This acts like a promise.
func NewOutput() (Output, func(interface{}), func(error)) {
	out := newOutputState(anyType)

	resolve := func(v interface{}) {
		out.resolve(v, true)
	}
	reject := func(err error) {
		out.reject(err)
	}

	return AnyOutput{out}, resolve, reject
}

var contextType = reflect.TypeOf((*context.Context)(nil)).Elem()
var errorType = reflect.TypeOf((*error)(nil)).Elem()

func makeContextful(fn interface{}, elementType reflect.Type) interface{} {
	fv := reflect.ValueOf(fn)
	if fv.Kind() != reflect.Func {
		panic(errors.New("applier must be a function"))
	}

	ft := fv.Type()
	if ft.NumIn() != 1 || !elementType.AssignableTo(ft.In(0)) {
		panic(errors.Errorf("applier must have 1 input parameter assignable from %v", elementType))
	}

	var outs []reflect.Type
	switch ft.NumOut() {
	case 1:
		// Okay
		outs = []reflect.Type{ft.Out(0)}
	case 2:
		// Second out parameter must be of type error
		if !ft.Out(1).AssignableTo(errorType) {
			panic(errors.New("applier's second return type must be assignable to error"))
		}
		outs = []reflect.Type{ft.Out(0), ft.Out(1)}
	default:
		panic(errors.New("appplier must return exactly one or two values"))
	}

	ins := []reflect.Type{contextType, ft.In(0)}
	contextfulType := reflect.FuncOf(ins, outs, ft.IsVariadic())
	contextfulFunc := reflect.MakeFunc(contextfulType, func(args []reflect.Value) []reflect.Value {
		// Slice off the context argument and call the applier.
		return fv.Call(args[1:])
	})
	return contextfulFunc.Interface()
}

func checkApplier(fn interface{}, elementType reflect.Type) reflect.Value {
	fv := reflect.ValueOf(fn)
	if fv.Kind() != reflect.Func {
		panic(errors.New("applier must be a function"))
	}

	ft := fv.Type()
	if ft.NumIn() != 2 || !contextType.AssignableTo(ft.In(0)) || !elementType.AssignableTo(ft.In(1)) {
		panic(errors.Errorf("applier's input parameters must be assignable from %v and %v", contextType, elementType))
	}

	switch ft.NumOut() {
	case 1:
		// Okay
	case 2:
		// Second out parameter must be of type error
		if !ft.Out(1).AssignableTo(errorType) {
			panic(errors.New("applier's second return type must be assignable to error"))
		}
	default:
		panic(errors.New("appplier must return exactly one or two values"))
	}

	// Okay
	return fv
}

// Apply transforms the data of the output property using the applier func. The result remains an output
// property, and accumulates all implicated dependencies, so that resources can be properly tracked using a DAG.
// This function does not block awaiting the value; instead, it spawns a Goroutine that will await its availability.
//
// nolint: interfacer
func (o *OutputState) Apply(applier func(interface{}) (interface{}, error)) AnyOutput {
	return o.ApplyWithContext(context.Background(), func(_ context.Context, v interface{}) (interface{}, error) {
		return applier(v)
	})
}

// ApplyWithContext transforms the data of the output property using the applier func. The result remains an output
// property, and accumulates all implicated dependencies, so that resources can be properly tracked using a DAG.
// This function does not block awaiting the value; instead, it spawns a Goroutine that will await its availability.
//
// nolint: interfacer
func (o *OutputState) ApplyWithContext(ctx context.Context, applier func(context.Context, interface{}) (interface{}, error)) AnyOutput {
	return o.ApplyTWithContext(ctx, applier).(AnyOutput)
}

// ApplyT transforms the data of the output property using the applier func. The result remains an output
// property, and accumulates all implicated dependencies, so that resources can be properly tracked using a DAG.
// This function does not block awaiting the value; instead, it spawns a Goroutine that will await its availability.
//
// The applier function must have one of the following signatures:
//
//    func (v U) T
//    func (v U) (T, error)
//
// U must be assignable from the ElementType of the Output. If T is a type that has a registered Output type, the
// result of ApplyT will be of the registered Output type, and can be used in an appropriate type assertion:
//
//    stringOutput := pulumi.String("hello").ToStringOutput()
//    intOutput := stringOutput.ApplyT(func(v string) int {
//        return len(v)
//    }).(pulumi.IntOutput)
//
// Otherwise, the result will be of type AnyOutput:
//
//    stringOutput := pulumi.String("hello").ToStringOutput()
//    intOutput := stringOutput.ApplyT(func(v string) []rune {
//        return []rune(v)
//    }).(pulumi.AnyOutput)
//
// nolint: interfacer
func (o *OutputState) ApplyT(applier interface{}) Output {
	return o.ApplyTWithContext(context.Background(), makeContextful(applier, o.elementType()))
}

var anyOutputType = reflect.TypeOf((*AnyOutput)(nil)).Elem()

// ApplyTWithContext transforms the data of the output property using the applier func. The result remains an output
// property, and accumulates all implicated dependencies, so that resources can be properly tracked using a DAG.
// This function does not block awaiting the value; instead, it spawns a Goroutine that will await its availability.
// The provided context can be used to reject the output as canceled.
//
// The applier function must have one of the following signatures:
//
//    func (ctx context.Context, v U) T
//    func (ctx context.Context, v U) (T, error)
//
// U must be assignable from the ElementType of the Output. If T is a type that has a registered Output type, the
// result of ApplyT will be of the registered Output type, and can be used in an appropriate type assertion:
//
//    stringOutput := pulumi.String("hello").ToStringOutput()
//    intOutput := stringOutput.ApplyTWithContext(func(_ context.Context, v string) int {
//        return len(v)
//    }).(pulumi.IntOutput)
//
// Otherwise, the result will be of type AnyOutput:
//
//    stringOutput := pulumi.String("hello").ToStringOutput()
//    intOutput := stringOutput.ApplyT(func(_ context.Context, v string) []rune {
//        return []rune(v)
//    }).(pulumi.AnyOutput)
//
// nolint: interfacer
func (o *OutputState) ApplyTWithContext(ctx context.Context, applier interface{}) Output {
	fn := checkApplier(applier, o.elementType())

	resultType := anyOutputType
	if ot, ok := concreteTypeToOutputType.Load(fn.Type().Out(0)); ok {
		resultType = ot.(reflect.Type)
	}

	result := newOutput(resultType, o.dependencies()...)
	go func() {
		v, known, err := o.await(ctx)
		if err != nil || !known {
			result.fulfill(nil, known, err)
			return
		}

		// If we have a known value, run the applier to transform it.
		results := fn.Call([]reflect.Value{reflect.ValueOf(ctx), reflect.ValueOf(v)})
		if len(results) == 2 && !results[1].IsNil() {
			result.reject(results[1].Interface().(error))
			return
		}

		// Fulfill the result.
		result.fulfill(results[0].Interface(), true, nil)
	}()
	return result
}

// All returns an ArrayOutput that will resolve when all of the provided inputs will resolve. Each element of the
// array will contain the resolved value of the corresponding output. The output will be rejected if any of the inputs
// is rejected.
func All(inputs ...Input) ArrayOutput {
	return AllWithContext(context.Background(), inputs...)
}

// AllWithContext returns an ArrayOutput that will resolve when all of the provided inputs will resolve. Each
// element of the array will contain the resolved value of the corresponding output. The output will be rejected if any
// of the inputs is rejected.
func AllWithContext(ctx context.Context, inputs ...Input) ArrayOutput {
	return ToOutputWithContext(ctx, Array(inputs)).(ArrayOutput)
}

func gatherDependencySet(input reflect.Value, deps map[Resource]struct{}) {
	inputV := input.Interface()

	// If the input does not implement the Input interface, it has no dependencies.
	if _, ok := inputV.(Input); !ok {
		return
	}

	// Check for an Output that we can pull dependencies off of.
	if output, ok := inputV.(Output); ok {
		for _, d := range output.dependencies() {
			deps[d] = struct{}{}
		}
		return
	}

	switch input.Kind() {
	case reflect.Struct:
		numFields := input.Type().NumField()
		for i := 0; i < numFields; i++ {
			gatherDependencySet(input.Field(i), deps)
		}
	case reflect.Slice:
		l := input.Len()
		for i := 0; i < l; i++ {
			gatherDependencySet(input.Index(i), deps)
		}
	case reflect.Map:
		iter := input.MapRange()
		for iter.Next() {
			gatherDependencySet(iter.Key(), deps)
			gatherDependencySet(iter.Value(), deps)
		}
	}
}

func gatherDependencies(input Input) []Resource {
	depSet := make(map[Resource]struct{})
	gatherDependencySet(reflect.ValueOf(input), depSet)

	if len(depSet) == 0 {
		return nil
	}

	deps := make([]Resource, 0, len(depSet))
	for d := range depSet {
		deps = append(deps, d)
	}
	return deps
}

func awaitStructFields(ctx context.Context, input reflect.Value, resolved reflect.Value) (bool, error) {
	inputType, resolvedType := input.Type(), resolved.Type()

	contract.Assert(inputType.Kind() == reflect.Struct)
	contract.Assert(resolvedType.Kind() == reflect.Struct)

	// We need to map the fields of the input struct to those of the output struct. We'll build a mapping from name to
	// to field index for the resolved type up front.
	nameToIndex := map[string]int{}
	numFields := resolvedType.NumField()
	for i := 0; i < numFields; i++ {
		nameToIndex[resolvedType.Field(i).Name] = i
	}

	// Now, resolve each field in the input.
	numFields = inputType.NumField()
	for i := 0; i < numFields; i++ {
		fieldName := inputType.Field(i).Name
		j, ok := nameToIndex[fieldName]
		if !ok {
			err := errors.Errorf("unknown field %v when resolving %v to %v", fieldName, inputType, resolvedType)
			return true, err
		}

		// Delete the mapping to indicate that we've resolved the field.
		delete(nameToIndex, fieldName)

		known, err := awaitInputs(ctx, input.Field(i), resolved.Field(j))
		if err != nil || !known {
			return known, err
		}
	}

	// Check for unassigned fields.
	if len(nameToIndex) != 0 {
		missing := make([]string, 0, len(nameToIndex))
		for name := range nameToIndex {
			missing = append(missing, name)
		}
		err := errors.Errorf("missing fields %s when resolving %v to %v", strings.Join(missing, ", "), inputType,
			resolvedType)
		return true, err
	}

	return true, nil
}

func awaitInputs(ctx context.Context, input reflect.Value, resolved reflect.Value) (bool, error) {
	contract.Assert(resolved.CanSet())
	contract.Assert(input.IsValid())
	contract.Assert(input.CanInterface())

	inputV := input.Interface()

	// If the input is an Output, await it.
	if out, ok := inputV.(Output); ok {
		v, known, err := out.await(ctx)
		if err != nil || !known {
			return known, err
		}
		input, inputV = reflect.ValueOf(v), v
	}

	// If we have no value for the result, we're done.
	if inputV == nil {
		return true, nil
	}

	// If the dest type is a pointer, allocate storage for the result.
	if resolved.Kind() == reflect.Ptr {
		elem := reflect.New(resolved.Type().Elem())
		resolved.Set(elem)
		resolved = elem.Elem()
	}

	// If the input does not implement the Input interface, we will stop here.
	asInput, ok := inputV.(Input)
	if !ok {
		if !input.Type().AssignableTo(resolved.Type()) {
			return true, errors.Errorf("cannot resolve a %v to a %v", resolved.Type(), input.Type())
		}
		resolved.Set(input)
		return true, nil
	}

	// Check for some well-known types.
	switch inputV := inputV.(type) {
	case *archive, *asset:
		// These are already fully-resolved.
		input = reflect.ValueOf(inputV)
		if resolved.Kind() != reflect.Ptr && resolved.Kind() != reflect.Interface {
			input = input.Elem()
		}
		resolved.Set(input)
		return true, nil
	}

	inputType, resolvedType := input.Type(), resolved.Type()

	// If the input is an interface, poke through to its concrete type.
	if inputType.Kind() == reflect.Interface {
		input = input.Elem()
		inputType = input.Type()
	}

	// If the dest type is an interface, we may not be able to assign to it.
	if resolvedType.Kind() == reflect.Interface {
		// Pull the element type from the input type.
		elementType := asInput.ElementType()
		if !elementType.AssignableTo(resolvedType) {
			return false, errors.Errorf("cannot resolve a %v to a %v", resolvedType, elementType)
		}

		// Create a new resolved value using the element type.
		iface := resolved
		resolved, resolvedType = reflect.New(elementType).Elem(), elementType
		defer func() { iface.Set(resolved) }()
	}

	// Now, examine the type and continue accordingly.
	if inputType.Kind() != resolvedType.Kind() {
		return false, errors.Errorf("cannot resolve a %v to a %v", resolvedType, inputType)
	}

	switch inputType.Kind() {
	case reflect.Struct:
		return awaitStructFields(ctx, input, resolved)
	case reflect.Slice:
		l := input.Len()
		slice := reflect.MakeSlice(resolvedType, l, l)

		for i := 0; i < l; i++ {
			eknown, err := awaitInputs(ctx, input.Index(i), slice.Index(i))
			if err != nil || !eknown {
				return eknown, err
			}
		}

		resolved.Set(slice)
		return true, nil
	case reflect.Map:
		result := reflect.MakeMap(resolvedType)
		resolvedKeyType, resolvedValueType := resolvedType.Key(), resolvedType.Elem()

		iter := input.MapRange()
		for iter.Next() {
			kv := reflect.New(resolvedKeyType).Elem()
			known, err := awaitInputs(ctx, iter.Key(), kv)
			if err != nil || !known {
				return known, err
			}

			vv := reflect.New(resolvedValueType).Elem()
			known, err = awaitInputs(ctx, iter.Value(), vv)
			if err != nil || !known {
				return known, err
			}

			result.SetMapIndex(kv, vv)
		}

		resolved.Set(result)
		return true, nil
	default:
		if !inputType.AssignableTo(resolvedType) {
			if !inputType.ConvertibleTo(resolvedType) {
				return true, errors.Errorf("cannot resolve a %v to a %v", resolvedType, inputType)
			}
			input = input.Convert(resolvedType)
		}
		resolved.Set(input)
		return true, nil
	}
}

// ToOutput returns an Output that will resolve when all Inputs contained in the given Input have resolved.
func ToOutput(input Input) Output {
	return ToOutputWithContext(context.Background(), input)
}

// ToOutputWithContext returns an Output that will resolve when all Outputs contained in the given Input have
// resolved.
func ToOutputWithContext(ctx context.Context, input Input) Output {
	elementType := input.ElementType()

	resultType := anyOutputType
	if ot, ok := concreteTypeToOutputType.Load(elementType); ok {
		resultType = ot.(reflect.Type)
	}

	result := newOutput(resultType, gatherDependencies(input)...)
	go func() {
		element := reflect.New(elementType).Elem()

		known, err := awaitInputs(ctx, reflect.ValueOf(input), element)
		if err != nil || !known {
			result.fulfill(nil, known, err)
			return
		}

		result.resolve(element.Interface(), true)
	}()
	return result
}

// Input is the type of a generic input value for a Pulumi resource. This type is used in conjunction with Output
// to provide polymorphism over strongly-typed input values.
//
// The intended pattern for nested Pulumi value types is to define an input interface and a plain, input, and output
// variant of the value type that implement the input interface.
//
// For example, given a nested Pulumi value type with the following shape:
//
//     type Nested struct {
//         Foo int
//         Bar string
//     }
//
// We would define the following:
//
//     var nestedType = reflect.TypeOf((*Nested)(nil)).Elem()
//
//     type NestedInput interface {
//         pulumi.Input
//
//         ToNestedOutput() NestedOutput
//         ToNestedOutputWithContext(context.Context) NestedOutput
//     }
//
//     type Nested struct {
//         Foo int `pulumi:"foo"`
//         Bar string `pulumi:"bar"`
//     }
//
//     type NestedInputValue struct {
//         Foo pulumi.IntInput `pulumi:"foo"`
//         Bar pulumi.StringInput `pulumi:"bar"`
//     }
//
//     func (NestedInputValue) ElementType() reflect.Type {
//         return nestedType
//     }
//
//     func (v NestedInputValue) ToNestedOutput() NestedOutput {
//         return pulumi.ToOutput(v).(NestedOutput)
//     }
//
//     func (v NestedInputValue) ToNestedOutputWithContext(ctx context.Context) NestedOutput {
//         return pulumi.ToOutputWithContext(ctx, v).(NestedOutput)
//     }
//
//     type NestedOutput struct { *pulumi.OutputState }
//
//     func (NestedOutput) ElementType() reflect.Type {
//         return nestedType
//     }
//
//     func (o NestedOutput) ToNestedOutput() NestedOutput {
//         return o
//     }
//
//     func (o NestedOutput) ToNestedOutputWithContext(ctx context.Context) NestedOutput {
//         return o
//     }
//
type Input interface {
	ElementType() reflect.Type
}

var anyType = reflect.TypeOf((*interface{})(nil)).Elem()

func Any(v interface{}) AnyOutput {
	out, resolve, _ := NewOutput()
	resolve(v)
	return out.(AnyOutput)
}

type AnyOutput struct{ *OutputState }

func (AnyOutput) ElementType() reflect.Type {
	return anyType
}

func (o IDOutput) awaitID(ctx context.Context) (ID, bool, error) {
	id, known, err := o.await(ctx)
	if !known || err != nil {
		return "", known, err
	}
	return ID(convert(id, stringType).(string)), true, nil
}

func (o URNOutput) awaitURN(ctx context.Context) (URN, bool, error) {
	id, known, err := o.await(ctx)
	if !known || err != nil {
		return "", known, err
	}
	return URN(convert(id, stringType).(string)), true, nil
}

func convert(v interface{}, to reflect.Type) interface{} {
	rv := reflect.ValueOf(v)
	if !rv.Type().ConvertibleTo(to) {
		panic(errors.Errorf("cannot convert output value of type %s to %s", rv.Type(), to))
	}
	return rv.Convert(to).Interface()
}
