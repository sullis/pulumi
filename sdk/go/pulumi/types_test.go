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

// nolint: lll
package pulumi

import (
	"context"
	"reflect"
	"testing"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
)

func await(out Output) (interface{}, bool, error) {
	return out.await(context.Background())
}

func assertApplied(t *testing.T, out Output) {
	_, known, err := await(out)
	assert.True(t, known)
	assert.Nil(t, err)
}

func newIntOutput() IntOutput {
	return IntOutput{newOutputState(reflect.TypeOf(42))}
}

func TestBasicOutputs(t *testing.T) {
	// Just test basic resolve and reject functionality.
	{
		out, resolve, _ := NewOutput()
		go func() {
			resolve(42)
		}()
		v, known, err := await(out)
		assert.Nil(t, err)
		assert.True(t, known)
		assert.NotNil(t, v)
		assert.Equal(t, 42, v.(int))
	}
	{
		out, _, reject := NewOutput()
		go func() {
			reject(errors.New("boom"))
		}()
		v, _, err := await(out)
		assert.NotNil(t, err)
		assert.Nil(t, v)
	}
}

func TestArrayOutputs(t *testing.T) {
	out := ArrayOutput{newOutputState(reflect.TypeOf([]interface{}{}))}
	go func() {
		out.resolve([]interface{}{nil, 0, "x"}, true)
	}()
	{
		assertApplied(t, out.ApplyT(func(arr []interface{}) (interface{}, error) {
			assert.NotNil(t, arr)
			if assert.Equal(t, 3, len(arr)) {
				assert.Equal(t, nil, arr[0])
				assert.Equal(t, 0, arr[1])
				assert.Equal(t, "x", arr[2])
			}
			return nil, nil
		}))
	}
}

func TestBoolOutputs(t *testing.T) {
	out := BoolOutput{newOutputState(reflect.TypeOf(false))}
	go func() {
		out.resolve(true, true)
	}()
	{
		assertApplied(t, out.ApplyT(func(v bool) (interface{}, error) {
			assert.True(t, v)
			return nil, nil
		}))
	}
}

func TestMapOutputs(t *testing.T) {
	out := MapOutput{newOutputState(reflect.TypeOf(map[string]interface{}{}))}
	go func() {
		out.resolve(map[string]interface{}{
			"x": 1,
			"y": false,
			"z": "abc",
		}, true)
	}()
	{
		assertApplied(t, out.ApplyT(func(v map[string]interface{}) (interface{}, error) {
			assert.NotNil(t, v)
			assert.Equal(t, 1, v["x"])
			assert.Equal(t, false, v["y"])
			assert.Equal(t, "abc", v["z"])
			return nil, nil
		}))
	}
}

func TestNumberOutputs(t *testing.T) {
	out := Float64Output{newOutputState(reflect.TypeOf(float64(0)))}
	go func() {
		out.resolve(42.345, true)
	}()
	{
		assertApplied(t, out.ApplyT(func(v float64) (interface{}, error) {
			assert.Equal(t, 42.345, v)
			return nil, nil
		}))
	}
}

func TestStringOutputs(t *testing.T) {
	out := StringOutput{newOutputState(reflect.TypeOf(""))}
	go func() {
		out.resolve("a stringy output", true)
	}()
	{
		assertApplied(t, out.ApplyT(func(v string) (interface{}, error) {
			assert.Equal(t, "a stringy output", v)
			return nil, nil
		}))
	}
}

func TestResolveOutputToOutput(t *testing.T) {
	// Test that resolving an output to an output yields the value, not the output.
	{
		out, resolve, _ := NewOutput()
		go func() {
			other, resolveOther, _ := NewOutput()
			resolve(other)
			go func() { resolveOther(99) }()
		}()
		assertApplied(t, out.ApplyT(func(v interface{}) (interface{}, error) {
			assert.Equal(t, v, 99)
			return nil, nil
		}))
	}
	// Similarly, test that resolving an output to a rejected output yields an error.
	{
		out, resolve, _ := NewOutput()
		go func() {
			other, _, rejectOther := NewOutput()
			resolve(other)
			go func() { rejectOther(errors.New("boom")) }()
		}()
		v, _, err := await(out)
		assert.NotNil(t, err)
		assert.Nil(t, v)
	}
}

// Test that ToOutput works with a struct type.
func TestToOutputStruct(t *testing.T) {
	out := ToOutput(nestedTypeArgs{Foo: String("bar"), Bar: Int(42)})
	_, ok := out.(nestedTypeInput)
	assert.True(t, ok)

	v, known, err := await(out)
	assert.True(t, known)
	assert.NoError(t, err)
	assert.Equal(t, nestedType{Foo: "bar", Bar: 42}, v)

	out = ToOutput(out)
	_, ok = out.(nestedTypeInput)
	assert.True(t, ok)

	v, known, err = await(out)
	assert.True(t, known)
	assert.NoError(t, err)
	assert.Equal(t, nestedType{Foo: "bar", Bar: 42}, v)

	out = ToOutput(nestedTypeArgs{Foo: ToOutput(String("bar")).(StringInput), Bar: ToOutput(Int(42)).(IntInput)})
	_, ok = out.(nestedTypeInput)
	assert.True(t, ok)

	v, known, err = await(out)
	assert.True(t, known)
	assert.NoError(t, err)
	assert.Equal(t, nestedType{Foo: "bar", Bar: 42}, v)
}
