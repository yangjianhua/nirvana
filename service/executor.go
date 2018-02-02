/*
Copyright 2017 Caicloud Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package service

import (
	"context"
	"fmt"
	"io"
	"path"
	"reflect"
	"runtime"
	"sort"

	"github.com/caicloud/nirvana/definition"
	"github.com/caicloud/nirvana/log"
	"github.com/caicloud/nirvana/service/router"
)

type inspector struct {
	path      string
	logger    log.Logger
	executors map[string][]*executor
}

func newInspector(path string, logger log.Logger) *inspector {
	return &inspector{
		path:      path,
		logger:    logger,
		executors: map[string][]*executor{},
	}
}

func (i *inspector) addDefinition(d definition.Definition) error {
	method := HTTPMethodFor(d.Method)
	if method == "" {
		return definitionNoMethod.Error(d.Method, i.path)
	}
	if len(d.Consumes) <= 0 {
		return definitionNoConsumes.Error(d.Method, i.path)
	}
	if len(d.Produces) <= 0 {
		return definitionNoProduces.Error(d.Method, i.path)
	}
	if d.Function == nil {
		return definitionNoFunction.Error(d.Method, i.path)
	}
	value := reflect.ValueOf(d.Function)
	if value.Kind() != reflect.Func {
		return definitionInvalidFunctionType.Error(value.Type(), d.Method, i.path)
	}
	c := &executor{
		logger:   i.logger,
		method:   method,
		code:     HTTPCodeFor(d.Method),
		function: value,
	}
	consumeAll := false
	consumes := map[string]bool{}
	for _, ct := range d.Consumes {
		if ct == definition.MIMEAll {
			consumeAll = true
			continue
		}
		if consumer := ConsumerFor(ct); consumer != nil {
			c.consumers = append(c.consumers, consumer)
			consumes[consumer.ContentType()] = true
		} else {
			return definitionNoConsumer.Error(ct, d.Method, i.path)
		}
	}
	if consumeAll {
		// Add remaining consumers to executor.
		for _, consumer := range AllConsumers() {
			if !consumes[consumer.ContentType()] {
				c.consumers = append(c.consumers, consumer)
			}
		}
	}
	produceAll := false
	produces := map[string]bool{}
	for _, ct := range d.Produces {
		if ct == definition.MIMEAll {
			produceAll = true
			continue
		}
		if producer := ProducerFor(ct); producer != nil {
			c.producers = append(c.producers, producer)
			produces[producer.ContentType()] = true
		} else {
			return definitionNoProducer.Error(ct, d.Method, i.path)
		}
	}
	if produceAll {
		// Add remaining producers to executor.
		for _, producer := range AllProducers() {
			if !produces[producer.ContentType()] {
				c.producers = append(c.producers, producer)
			}
		}
	}
	// Get func name and file position.
	f := runtime.FuncForPC(value.Pointer())
	file, line := f.FileLine(value.Pointer())
	// Function name examples:
	// 1. Common function: api.CreateSomething(create.go#30)
	// 2. Anonymous function: api.glob..func1(create.go#30)
	//    Anonymous function names are generated by go. Don't explore their meaning.
	funcName := fmt.Sprintf("%s(%s#%d)", path.Base(f.Name()), path.Base(file), line)
	ps, err := i.generateParameters(funcName, value.Type(), d.Parameters)
	if err != nil {
		return err
	}
	c.parameters = ps
	rs, err := i.generateResults(funcName, value.Type(), d.Results)
	if err != nil {
		return err
	}
	c.results = rs
	if err := i.conflictCheck(c); err != nil {
		return err
	}
	i.executors[method] = append(i.executors[method], c)
	return nil
}

func (i *inspector) conflictCheck(c *executor) error {
	cs := i.executors[c.method]
	if len(cs) <= 0 {
		return nil
	}
	ctMap := map[string]bool{}
	for _, extant := range cs {
		result := extant.ctMap()
		for k, vs := range result {
			for _, v := range vs {
				ctMap[k+":"+v] = true
			}
		}
	}
	cMap := c.ctMap()
	for k, vs := range cMap {
		for _, v := range vs {
			if !ctMap[k+":"+v] {
				return definitionConflict.Error(k, v, c.method, i.path)
			}
		}
	}
	return nil
}

func (i *inspector) generateParameters(funcName string, typ reflect.Type, ps []definition.Parameter) ([]parameter, error) {
	if typ.NumIn() != len(ps) {
		return nil, definitionUnmatchedParameters.Error(funcName, typ.NumIn(), len(ps), i.path)
	}
	parameters := make([]parameter, 0, len(ps))
	for index, p := range ps {
		generator := ParameterGeneratorFor(p.Source)
		if generator == nil {
			return nil, noParameterGenerator.Error(p.Source)
		}

		param := parameter{
			name:         p.Name,
			defaultValue: p.Default,
			generator:    generator,
			operators:    p.Operators,
		}
		if len(p.Operators) <= 0 {
			param.targetType = typ.In(index)
		} else {
			param.targetType = p.Operators[0].In()
		}
		if err := generator.Validate(param.name, param.defaultValue, param.targetType); err != nil {
			// Order from 0 is odd. So index+1.
			i.logger.Errorf("Can't validate %s parameter of function %s: %s", order(index+1), funcName, err.Error())
			return nil, err
		}
		if len(param.operators) > 0 {
			if err := i.validateOperators(param.targetType, typ.In(index), param.operators); err != nil {
				i.logger.Errorf("Can't validate operators for %s parameter of function %s: %s", order(index+1), funcName, err.Error())
				return nil, err
			}
		}
		parameters = append(parameters, param)
	}
	return parameters, nil
}

func (i *inspector) generateResults(funcName string, typ reflect.Type, rs []definition.Result) ([]result, error) {
	if typ.NumOut() != len(rs) {
		return nil, definitionUnmatchedResults.Error(funcName, typ.NumOut(), len(rs), i.path)
	}
	results := make([]result, 0, len(rs))
	for index, r := range rs {
		handler := DestinationHandlerFor(r.Destination)
		if handler == nil {
			return nil, noDestinationHandler.Error(r.Destination)
		}
		result := result{
			index:     index,
			handler:   handler,
			operators: r.Operators,
		}
		outType := typ.Out(index)
		if len(result.operators) > 0 {
			LastOperatorOutType := result.operators[len(result.operators)-1].Out()
			if err := i.validateOperators(outType, LastOperatorOutType, result.operators); err != nil {
				i.logger.Errorf("Can't validate operators for %s result of function %s: %s", order(index+1), funcName, err.Error())
				return nil, err
			}
			outType = LastOperatorOutType
		}
		if err := handler.Validate(outType); err != nil {
			// Order from 0 is odd. So index+1.
			i.logger.Errorf("Can't validate %s result of function %s: %s", order(index+1), funcName, err.Error())
			return nil, err
		}
		results = append(results, result)
	}
	sort.Sort(resultsSorter(results))
	return results, nil

}

// validateOperators checks if the chain is valid:
//   in -> operators[0].In()
//   operators[0].Out() -> operators[1].In()
//   ...
//   operators[N].Out() -> out
func (i *inspector) validateOperators(in, out reflect.Type, operators []definition.Operator) error {
	if len(operators) <= 0 {
		return nil
	}
	index := 0
	for ; index < len(operators); index++ {
		operator := operators[index]
		if !in.AssignableTo(operator.In()) {
			// The out type of operator[index-1] is not compatible to operator[index].
			return invalidOperatorInType.Error(in, order(index+1))
		}
		in = operator.Out()
	}
	typ := operators[index-1].Out()
	if !typ.AssignableTo(out) {
		// The last operator is not compatible to out type.
		return invalidOperatorOutType.Error(order(index), out)
	}
	return nil
}

type resultsSorter []result

// Len is the number of elements in the collection.
func (s resultsSorter) Len() int {
	return len(s)
}

// Less reports whether the element with
// index i should sort before the element with index j.
func (s resultsSorter) Less(i, j int) bool {
	return s[i].handler.Priority() < s[j].handler.Priority()
}

// Swap swaps the elements with indexes i and j.
func (s resultsSorter) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

// Inspect finds a valid executor to execute target context.
func (i *inspector) Inspect(ctx context.Context) (router.Executor, error) {
	req := HTTPContextFrom(ctx).Request()
	if req == nil {
		return nil, noContext.Error()
	}
	executors := []*executor{}
	if cs, ok := i.executors[req.Method]; ok && len(cs) > 0 {
		executors = append(executors, cs...)
	}
	if len(executors) <= 0 {
		return nil, noExecutorForMethod.Error()
	}
	ct, err := ContentType(req)
	if err != nil {
		return nil, err
	}
	accepted := 0
	for i, c := range executors {
		if c.acceptable(ct) {
			if accepted != i {
				executors[accepted] = c
			}
			accepted++
		}
	}
	if accepted <= 0 {
		return nil, noExecutorForContentType.Error()
	}
	ats, err := AcceptTypes(req)
	if err != nil {
		return nil, err
	}
	executors = executors[:accepted]
	var target *executor
	for _, c := range executors {
		if c.producible(ats) {
			target = c
			break
		}
	}
	if target == nil {
		for _, at := range ats {
			if at == definition.MIMEAll {
				target = executors[0]
			}
		}
	}
	if target == nil {
		return nil, noExecutorToProduce.Error()
	}
	return target, nil
}

type executor struct {
	logger     log.Logger
	method     string
	code       int
	consumers  []Consumer
	producers  []Producer
	parameters []parameter
	results    []result
	function   reflect.Value
}

type parameter struct {
	name         string
	targetType   reflect.Type
	defaultValue interface{}
	generator    ParameterGenerator
	operators    []definition.Operator
}

type result struct {
	index     int
	handler   DestinationHandler
	operators []definition.Operator
}

func (e *executor) ctMap() map[string][]string {
	result := map[string][]string{}
	for _, c := range e.consumers {
		for _, p := range e.producers {
			ct := c.ContentType()
			result[ct] = append(result[ct], p.ContentType())
		}
	}
	return result
}

func (e *executor) acceptable(ct string) bool {
	for _, c := range e.consumers {
		if c.ContentType() == ct {
			return true
		}
	}
	return false
}

func (e *executor) producible(ats []string) bool {
	for _, at := range ats {
		for _, c := range e.producers {
			if c.ContentType() == at {
				return true
			}
		}
	}
	return false
}

// Execute executes with context.
func (e *executor) Execute(ctx context.Context) (err error) {
	c := HTTPContextFrom(ctx)
	if c == nil {
		return noContext.Error()
	}
	paramValues := make([]reflect.Value, 0, len(e.parameters))
	for _, p := range e.parameters {
		result, err := p.generator.Generate(ctx, c.ValueContainer(), e.consumers, p.name, p.targetType)
		if err != nil {
			return writeError(ctx, e.producers, err)
		}
		if result == nil {
			if p.defaultValue != nil {
				result = p.defaultValue
			} else {
				result = reflect.Zero(p.targetType).Interface()
			}
		}
		for _, operator := range p.operators {
			result, err = operator.Operate(ctx, p.name, result)
			if err != nil {
				return writeError(ctx, e.producers, err)
			}
		}
		if result == nil {
			return writeError(ctx, e.producers, requiredField.Error(p.name, p.generator.Source()))
		} else if closer, ok := result.(io.Closer); ok {
			defer func() {
				if e := closer.Close(); e != nil && err == nil {
					// Need to print error here.
					err = e
				}
			}()
		}

		paramValues = append(paramValues, reflect.ValueOf(result))
	}
	resultValues := e.function.Call(paramValues)
	for _, r := range e.results {
		v := resultValues[r.index]
		data := v.Interface()
		for _, operator := range r.operators {
			newData, err := operator.Operate(ctx, string(r.handler.Destination()), data)
			if err != nil {
				return err
			}
			data = newData
		}
		if data != nil {
			if closer, ok := data.(io.Closer); ok {
				defer func() {
					if e := closer.Close(); e != nil && err == nil {
						// Need to print error here.
						err = e
					}
				}()
			}
		}
		goon, err := r.handler.Handle(ctx, e.producers, e.code, data)
		if err != nil {
			return err
		}
		if !goon {
			break
		}
	}
	resp := c.ResponseWriter()
	if resp.HeaderWritable() {
		resp.WriteHeader(e.code)
	}
	return nil
}

func order(i int) string {
	switch i % 10 {
	case 1:
		return fmt.Sprintf("%dst", i)
	case 2:
		return fmt.Sprintf("%dnd", i)
	case 3:
		return fmt.Sprintf("%drd", i)
	default:
		return fmt.Sprintf("%dth", i)
	}
}
