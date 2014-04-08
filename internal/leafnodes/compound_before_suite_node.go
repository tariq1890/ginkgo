package leafnodes

import (
	"bytes"
	"encoding/json"
	"github.com/onsi/ginkgo/internal/failer"
	"github.com/onsi/ginkgo/types"
	"io/ioutil"
	"net/http"
	"reflect"
	"time"
)

type RemoteStateState int

const (
	RemoteStateStateInvalid RemoteStateState = iota

	RemoteStateStatePending
	RemoteStateStatePassed
	RemoteStateStateFailed
	RemoteStateStateDisappeared
)

type RemoteState struct {
	Data  []byte
	State RemoteStateState
}

func (r RemoteState) ToJSON() []byte {
	data, _ := json.Marshal(r)
	return data
}

type compoundBeforeSuiteNode struct {
	runnerA *runner
	runnerB *runner

	ginkgoNode       int
	totalGinkgoNodes int
	syncHost         string

	data []byte

	outcome types.SpecState
	failure types.SpecFailure
	runTime time.Duration
}

func NewCompoundBeforeSuiteNode(bodyA interface{}, bodyB interface{}, codeLocation types.CodeLocation, timeout time.Duration, failer *failer.Failer, ginkgoNode int, totalGinkgoNodes int, syncHost string) SuiteNode {
	node := &compoundBeforeSuiteNode{
		ginkgoNode:       ginkgoNode,
		totalGinkgoNodes: totalGinkgoNodes,
		syncHost:         syncHost,
	}

	node.runnerA = newRunner(node.wrapA(bodyA), codeLocation, timeout, failer, types.SpecComponentTypeBeforeSuite, 0)
	node.runnerB = newRunner(node.wrapB(bodyB), codeLocation, timeout, failer, types.SpecComponentTypeBeforeSuite, 0)

	return node
}

func (node *compoundBeforeSuiteNode) Run() bool {
	t := time.Now()
	defer func() {
		node.runTime = time.Since(t)
	}()

	if node.ginkgoNode == 1 {
		node.outcome, node.failure = node.runA()
	} else {
		node.outcome, node.failure = node.waitForA()
	}

	if node.outcome != types.SpecStatePassed {
		return false
	}
	node.outcome, node.failure = node.runnerB.run()

	return node.outcome == types.SpecStatePassed
}

func (node *compoundBeforeSuiteNode) runA() (types.SpecState, types.SpecFailure) {
	outcome, failure := node.runnerA.run()

	if node.totalGinkgoNodes > 1 {
		state := RemoteStateStatePassed
		if outcome != types.SpecStatePassed {
			state = RemoteStateStateFailed
		}
		json := (RemoteState{
			Data:  node.data,
			State: state,
		}).ToJSON()
		http.Post(node.syncHost+"/BeforeSuiteState", "application/json", bytes.NewBuffer(json))
	}

	return outcome, failure
}

func (node *compoundBeforeSuiteNode) waitForA() (types.SpecState, types.SpecFailure) {
	failure := func(message string) types.SpecFailure {
		return types.SpecFailure{
			Message:               message,
			Location:              node.runnerA.codeLocation,
			ComponentType:         node.runnerA.nodeType,
			ComponentIndex:        node.runnerA.componentIndex,
			ComponentCodeLocation: node.runnerA.codeLocation,
		}
	}
	for {
		resp, err := http.Get(node.syncHost + "/BeforeSuiteState")
		if err != nil || resp.StatusCode != http.StatusOK {
			return types.SpecStateFailed, failure("Failed to fetch BeforeSuite state")
		}

		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return types.SpecStateFailed, failure("Failed to read BeforeSuite state")
		}
		resp.Body.Close()

		r := RemoteState{}
		err = json.Unmarshal(body, &r)
		if err != nil {
			return types.SpecStateFailed, failure("Failed to decode BeforeSuite state")
		}

		switch r.State {
		case RemoteStateStatePassed:
			node.data = r.Data
			return types.SpecStatePassed, types.SpecFailure{}
		case RemoteStateStateFailed:
			return types.SpecStateFailed, failure("BeforeSuite on Node 1 failed")
		case RemoteStateStateDisappeared:
			return types.SpecStateFailed, failure("Node 1 dissappeared before completing BeforeSuite")
		}

		time.Sleep(50 * time.Millisecond)
	}

	return types.SpecStateFailed, failure("Shouldn't get here!")
}

func (node *compoundBeforeSuiteNode) Passed() bool {
	return node.outcome == types.SpecStatePassed
}

func (node *compoundBeforeSuiteNode) Summary() *types.SetupSummary {
	return &types.SetupSummary{
		ComponentType: node.runnerA.nodeType,
		CodeLocation:  node.runnerA.codeLocation,
		State:         node.outcome,
		RunTime:       node.runTime,
		Failure:       node.failure,
	}
}

func (node *compoundBeforeSuiteNode) wrapA(bodyA interface{}) interface{} {
	typeA := reflect.TypeOf(bodyA)
	if typeA.Kind() != reflect.Func {
		panic("CompoundBeforeSuite expects a function as its first argument")
	}

	takesNothing := typeA.NumIn() == 0
	takesADoneChannel := typeA.NumIn() == 1 && typeA.In(0).Kind() == reflect.Chan && typeA.In(0).Elem().Kind() == reflect.Interface
	returnsBytes := typeA.NumOut() == 1 && typeA.Out(0).Kind() == reflect.Slice && typeA.Out(0).Elem().Kind() == reflect.Uint8

	if !((takesNothing || takesADoneChannel) && returnsBytes) {
		panic("CompoundBeforeSuite's first argument should be a function that returns []byte and either takes no arguments or takes a Done channel.")
	}

	if takesADoneChannel {
		return func(done chan<- interface{}) {
			out := reflect.ValueOf(bodyA).Call([]reflect.Value{reflect.ValueOf(done)})
			node.data = out[0].Interface().([]byte)
		}
	}

	return func() {
		out := reflect.ValueOf(bodyA).Call([]reflect.Value{})
		node.data = out[0].Interface().([]byte)
	}
}

func (node *compoundBeforeSuiteNode) wrapB(bodyB interface{}) interface{} {
	typeB := reflect.TypeOf(bodyB)
	if typeB.Kind() != reflect.Func {
		panic("CompoundBeforeSuite expects a function as its second argument")
	}

	returnsNothing := typeB.NumOut() == 0
	takesBytesOnly := typeB.NumIn() == 1 && typeB.In(0).Kind() == reflect.Slice && typeB.In(0).Elem().Kind() == reflect.Uint8
	takesBytesAndDone := typeB.NumIn() == 2 &&
		typeB.In(0).Kind() == reflect.Slice && typeB.In(0).Elem().Kind() == reflect.Uint8 &&
		typeB.In(1).Kind() == reflect.Chan && typeB.In(1).Elem().Kind() == reflect.Interface

	if !((takesBytesOnly || takesBytesAndDone) && returnsNothing) {
		panic("CompoundBeforeSuite's second argument should be a function that returns nothing and either takes []byte or ([]byte, Done)")
	}

	if takesBytesAndDone {
		return func(done chan<- interface{}) {
			reflect.ValueOf(bodyB).Call([]reflect.Value{reflect.ValueOf(node.data), reflect.ValueOf(done)})
		}
	}

	return func() {
		reflect.ValueOf(bodyB).Call([]reflect.Value{reflect.ValueOf(node.data)})
	}
}