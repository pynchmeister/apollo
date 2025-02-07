package dsl

import (
	"errors"
	"fmt"
	"io/ioutil"
	"math/big"
	"os"
	"path"
	"time"

	"github.com/chainbound/apollo/types"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hcldec"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/gocty"
)

var (
	ErrNoIntervalRealtime                 = errors.New("no interval defined for realtime method calls")
	ErrNoIntervalHistorical               = errors.New("no interval defined for historical method calls")
	ErrIntervalDefinedForHistoricalEvents = errors.New("interval defined for historical events")
)

// DynamicSchema represents the schema at different steps
// of evaluation. For each step, it contains methods to proceed to
// the next step with new information available.
type DynamicSchema struct {
	StartTime     int64                `hcl:"start_time,optional"`
	EndTime       int64                `hcl:"end_time,optional"`
	TimeInterval  int64                `hcl:"time_interval,optional"`
	StartBlock    int64                `hcl:"start_block,optional"`
	EndBlock      int64                `hcl:"end_block,optional"`
	BlockInterval int64                `hcl:"block_interval,optional"`
	Variables     map[string]cty.Value `hcl:"variables,optional"`

	// Represents the to-be-decoded queries / loops
	SchemaConfig hcl.Body `hcl:",remain"`

	// These queries will be added later.
	QuerySchemas []*QuerySchema

	// EvalContext is what defines the functions and variables that are available to the parser
	// and can thus be used by the schema. They will be different at each step of the execution.
	EvalContext *hcl.EvalContext
}

// LoopSchema defines a DSL loop block. It contains the items,
// and for every item a query is generated.
type LoopSchema struct {
	Items       []cty.Value `hcl:"items"`
	QuerySchema hcl.Body    `hcl:",remain"`
}

// QuerySchema defines a DSL query block.
type QuerySchema struct {
	Name  string      `hcl:"name,label"`
	Chain types.Chain `hcl:"chain"`

	// ContractSchemas holds an array of contract schemas
	ContractSchemas []*ContractSchema `hcl:"contract,block"`
	// EventSchemas holds an array of event schemas
	EventSchemas []*EventSchema `hcl:"event,block"`

	// The Save block also contains unknown options with hcl:"remain",
	// but we have to define it as a block here because it's mandatory
	Saves   Save     `hcl:"save,block"`
	Filters hcl.Body `hcl:"filter,remain"`

	// Every query can have its own block intervals,
	// since it can run on different chains.
	StartBlock    int64
	EndBlock      int64
	BlockInterval int64

	EvalContext *hcl.EvalContext
}

// EvalTransforms evaluates the transformation block per contract / top-level method.
// The identifier is the OutputName of the method or the name of the contract in other
// cases.
func (q *QuerySchema) EvalTransforms(tp types.ResultType, identifier string) error {
	if tp == types.GlobalEvent {
		for _, event := range q.EventSchemas {
			if event.Transforms == nil {
				return nil
			}

			if event.OutputName() == identifier {
				mv := make(map[string]cty.Value)
				diags := gohcl.DecodeBody(event.Transforms.Options, q.EvalContext, &mv)
				if diags.HasErrors() {
					return diags.Errs()[0]
				}

				for k, v := range mv {
					q.EvalContext.Variables[k] = v
				}
			}
		}
	} else {
		for _, c := range q.ContractSchemas {
			if c.Transforms == nil {
				return nil
			}

			if c.Address().String() == identifier {
				mv := make(map[string]cty.Value)
				diags := gohcl.DecodeBody(c.Transforms.Options, q.EvalContext, &mv)
				if diags.HasErrors() {
					return diags.Errs()[0]
				}

				for k, v := range mv {
					q.EvalContext.Variables[k] = v
				}
			}
		}
	}

	return nil
}

// EvalFilter evaluates the filter list, and if one of the items
// evaluates to false, it returns false and the evaluation should be stopped.
func (s *DynamicSchema) EvalFilter(queryName string) (bool, error) {
	filterspec := hcldec.AttrSpec{
		Name: "filter",
		Type: cty.List(cty.Bool),
	}

	var filters []bool
	for _, q := range s.QuerySchemas {
		if q.Name == queryName {
			if q.Filters == nil {
				return true, nil
			}

			v, diags := hcldec.Decode(q.Filters, &filterspec, q.EvalContext)
			if diags.HasErrors() {
				return false, diags.Errs()[0]
			}

			err := gocty.FromCtyValue(v, &filters)
			if err != nil {
				return false, err
			}
		}
	}

	// Check if all outputs evaluate to true, otherwise return false
	for _, result := range filters {
		if !result {
			return false, nil
		}
	}

	return true, nil
}

type ChainFunctionProvider interface {
	Balance(types.Chain, common.Address, *big.Int) (float64, error)
	TokenBalance(types.Chain, common.Address, common.Address, *big.Int) (float64, error)
	// Price(types.Chain, common.Address, common.Address, *big.Int) (float64, error)
}

// EvalSave updates the evaluation context, evaluates the transform blocks and then
// evaluates the save block. The results will be returned as a map.
func (s *DynamicSchema) EvalSave(provider ChainFunctionProvider, res types.CallResult) (map[string]cty.Value, error) {
	outputs := make(map[string]cty.Value)
	for _, q := range s.QuerySchemas {
		if q.Name == res.QueryName {
			if q.EvalContext.Variables == nil {
				q.EvalContext.Variables = make(map[string]cty.Value)
			}

			for k, v := range GenerateContextVars(res) {
				q.EvalContext.Variables[k] = v
			}

			for k, v := range BuildChainFunctions(provider, res.Chain, big.NewInt(int64(res.BlockNumber))) {
				q.EvalContext.Functions[k] = v
			}

			if err := q.EvalTransforms(res.Type, res.Identifier); err != nil {
				return nil, err
			}

			diags := gohcl.DecodeBody(q.Saves.Options, q.EvalContext, &outputs)
			if diags.HasErrors() {
				return nil, diags.Errs()[0]
			}
		}
	}

	ok, err := s.EvalFilter(res.QueryName)
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, nil
	}

	return outputs, nil
}

func (s DynamicSchema) Validate(opts types.ApolloOpts) error {
	hasMethods := false
	hasEvents := false
	for _, q := range s.QuerySchemas {
		for _, c := range q.ContractSchemas {
			hasMethods = len(c.Methods) > 0
			hasEvents = len(c.Events) > 0
		}
	}

	if hasMethods {
		if opts.Realtime {
			if s.BlockInterval == 0 && s.TimeInterval == 0 {
				return ErrNoIntervalRealtime
			}
		}

		if (s.StartBlock != 0 && s.EndBlock != 0) || (s.StartTime != 0 && s.EndTime != 0) {
			if s.BlockInterval == 0 && s.TimeInterval == 0 {
				return ErrNoIntervalHistorical
			}
		}
	}

	if hasEvents {
		if !opts.Realtime {
			if s.BlockInterval != 0 {
				return ErrIntervalDefinedForHistoricalEvents
			}

			if s.TimeInterval != 0 {
				return ErrIntervalDefinedForHistoricalEvents
			}
		}
	}

	return nil
}

func (q QuerySchema) HasGlobalEvents() bool {
	return len(q.EventSchemas) > 0
}

func (q QuerySchema) HasContractEvents() (hasContractEvents bool) {
	for _, c := range q.ContractSchemas {
		if len(c.Events) > 0 {
			hasContractEvents = true
		}
	}

	return
}

func (q QuerySchema) HasContractMethods() (hasContractMethods bool) {
	for _, c := range q.ContractSchemas {
		if len(c.Methods) > 0 {
			hasContractMethods = true
		}
	}

	return
}

type ContractSchema struct {
	Address_ string `hcl:"address"`
	AbiPath  string `hcl:"abi"`

	// ContractSchema can hold both methods
	// and events
	Methods []*MethodSchema `hcl:"method,block"`
	Events  []*EventSchema  `hcl:"event,block"`

	// Transform internally uses hcl:"remain",
	// because it has to work with previously fetched
	// data.
	Transforms *Transform `hcl:"transform,block"`

	// The ABI will get injected when decoding the schema
	Abi abi.ABI
}

func (c ContractSchema) Address() common.Address {
	return common.HexToAddress(c.Address_)
}

type MethodSchema struct {
	// BlockOffset is the block offset at which to call the method.
	// Only used when this method is a method that's supposed to be called
	// at a certain event.
	BlockOffset int64  `hcl:"block_offset,optional"`
	Name_       string `hcl:"name,label"`

	// Inputs_ contains the method input arguments. The names of the arguments
	// have to be the same as in the ABI.
	Inputs_ map[string]string `hcl:"inputs,optional"`
	// The method outputs we want to save. Any named outputs should be the same
	// as in the ABI.
	Outputs []string `hcl:"outputs"`
}

func (m MethodSchema) Name() string {
	return m.Name_
}

func (m MethodSchema) Inputs() map[string]string {
	return m.Inputs_
}

type EventSchema struct {
	Name_   string `hcl:"name,label"`
	AbiPath string `hcl:"abi,optional"`

	// The event outputs we want to save. They
	// have to be the same as in the ABI.
	Outputs_ []string `hcl:"outputs"`
	// Any optional methods we want to call at the event.
	Methods []*MethodSchema `hcl:"method,block"`

	// Transform internally uses hcl:"remain",
	// because it has to work with previously fetched
	// data.
	Transforms *Transform `hcl:"transform,block"`

	// The ABI will get injected when decoding the schema
	Abi abi.ABI
}

func (e EventSchema) Name() string {
	return e.Name_
}

func (e EventSchema) Outputs() []string {
	return e.Outputs_
}

func (e EventSchema) OutputName() string {
	return e.Name_ + "_events"
}

type Transform struct {
	// These should be decoded in a later step with different evaluation contexts,
	// because they should provide access to things like inputs, outputs,
	// block numbers, tx hashes etc.
	Options hcl.Body `hcl:",remain"`
}

type Save struct {
	// These should be decoded in a later step with different evaluation contexts,
	// because they should provide access to things like inputs, outputs,
	// block numbers, tx hashes etc.
	Options hcl.Body `hcl:",remain"`
}

// InitialContext returns the initial context at the start of evaluation.
// It has nothing but the most basic functions and variables.
func InitialContext() hcl.EvalContext {
	return hcl.EvalContext(hcl.EvalContext{
		Functions: Functions,
		Variables: map[string]cty.Value{
			"now": cty.NumberIntVal(time.Now().UnixMilli() / 1000),
		},
	})
}

// NewSchema returns a new DynamicSchema, loaded from confDir/schema.hcl.
// It will decode the top-level body with an initial evaluation context
// to provide access to custom functions. For each contract, it will also
// read and convert the json ABI file to an abi.ABI.
func NewSchema(confDir string) (*DynamicSchema, error) {
	schemaPath := path.Join(confDir, "schema.hcl")
	f, err := ioutil.ReadFile(schemaPath)
	if err != nil {
		return nil, err
	}

	file, diags := hclsyntax.ParseConfig(f, schemaPath, hcl.InitialPos)
	if diags.HasErrors() {
		return nil, diags.Errs()[0]
	}

	// Set up the inital context (access to upper, lower, etc)
	schemaContext := InitialContext()
	s := &DynamicSchema{
		EvalContext: &schemaContext,
	}

	// Decode ONLY the variables, that's the first thing we need
	diags = gohcl.DecodeBody(file.Body, &schemaContext, s)
	if diags.HasErrors() {
		return nil, diags.Errs()[0]
	}

	// Add the variables into the evaluation context
	for k, v := range s.Variables {
		s.EvalContext.Variables[k] = v
	}

	// This is the next step we need to decode. It's either a Loop or Queries.
	var topLevel struct {
		Loop    *LoopSchema    `hcl:"loop,block"`
		Queries []*QuerySchema `hcl:"query,block"`
	}

	// We decode into the topLevel struct with the variables available.
	diags = gohcl.DecodeBody(s.SchemaConfig, s.EvalContext, &topLevel)
	if diags.HasErrors() {
		return nil, diags.Errs()[0]
	}

	// If there are top-level queries, immediately save them
	s.QuerySchemas = append(s.QuerySchemas, topLevel.Queries...)

	// If there are loops, loop over the queries, decode them using
	// the loop variables in the evaluation context, and save the queries
	if topLevel.Loop != nil {
		for _, item := range topLevel.Loop.Items {
			var loopLevel struct {
				Queries []*QuerySchema `hcl:"query,block"`
			}

			newCtx := InitialContext()
			newCtx.Variables = map[string]cty.Value{"item": item}
			diags = gohcl.DecodeBody(topLevel.Loop.QuerySchema, &newCtx, &loopLevel)
			if diags.HasErrors() {
				return nil, diags.Errs()[0]
			}

			s.QuerySchemas = append(s.QuerySchemas, loopLevel.Queries...)
		}
	}

	// For every query, add the evaluation context (we need it later),
	// then parse and load the needed ABIs.
	for _, query := range s.QuerySchemas {
		query.EvalContext = s.EvalContext

		for _, event := range query.EventSchemas {
			f, err := os.Open(path.Join(confDir, event.AbiPath))
			if err != nil {
				return nil, fmt.Errorf("ParseV2: reading ABI file: %w", err)
			}

			abi, err := abi.JSON(f)
			if err != nil {
				return nil, fmt.Errorf("ParseV2: parsing ABI")
			}

			event.Abi = abi
		}

		for _, contract := range query.ContractSchemas {
			f, err := os.Open(path.Join(confDir, contract.AbiPath))
			if err != nil {
				return nil, fmt.Errorf("ParseV2: reading ABI file: %w", err)
			}

			abi, err := abi.JSON(f)
			if err != nil {
				return nil, fmt.Errorf("ParseV2: parsing ABI")
			}

			contract.Abi = abi
		}
	}

	return s, nil
}

// GenerateContextVars converts a CallResult into a map that can be added
// as context variables. This function is called after the first step (calling
// methods or parsing events) to generate variables to be used in the next steps
// (transform, filter, save).
func GenerateContextVars(cr types.CallResult) map[string]cty.Value {
	m := make(map[string]cty.Value)

	m["contract_address"], _ = gocty.ToCtyValue(cr.ContractAddress.String(), cty.String)
	m["blocknumber"], _ = gocty.ToCtyValue(cr.BlockNumber, cty.Number)
	m["timestamp"], _ = gocty.ToCtyValue(cr.Timestamp, cty.Number)
	m["block_hash"], _ = gocty.ToCtyValue(cr.BlockHash.String(), cty.String)
	m["chain"], _ = gocty.ToCtyValue(cr.Chain, cty.String)

	if cr.Type != types.Method {
		m["tx_hash"], _ = gocty.ToCtyValue(cr.TxHash.String(), cty.String)
		m["event_name"], _ = gocty.ToCtyValue(cr.EventName, cty.String)
		m["tx_index"], _ = gocty.ToCtyValue(cr.TxIndex, cty.Number)
	}

	for k, v := range cr.Inputs {
		switch v.(type) {
		case string:
			m[k], _ = gocty.ToCtyValue(v, cty.String)
		default:
			m[k], _ = gocty.ToCtyValue(v, cty.Number)
		}
	}

	for k, v := range cr.Outputs {
		switch v := v.(type) {
		case common.Address:
			m[k], _ = gocty.ToCtyValue(v.String(), cty.String)
		case string:
			m[k], _ = gocty.ToCtyValue(v, cty.String)
		default:
			m[k], _ = gocty.ToCtyValue(v, cty.Number)
		}
	}

	return m
}
