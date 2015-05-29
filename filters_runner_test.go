package main

import (
	"testing"

	"github.com/johann8384/libbeat/common"
	"github.com/johann8384/libbeat/filters"
	"github.com/johann8384/libbeat/filters/nop"

	"github.com/stretchr/testify/assert"
)

func loadPlugins() {
	filters.Filters.Register(filters.NopFilter, new(nop.Nop))
}

func TestFilterRunner(t *testing.T) {
	loadPlugins()

	output := make(chan common.MapStr, 10)

	filter1, err := new(nop.Nop).New("nop1", map[string]interface{}{})
	assert.Nil(t, err)

	filter2, err := new(nop.Nop).New("nop2", map[string]interface{}{})
	assert.Nil(t, err)

	runner := NewFilterRunner(output, []filters.FilterPlugin{filter1, filter2})
	assert.NotNil(t, runner)

	go runner.Run()

	runner.FiltersQueue <- common.MapStr{"hello": "world"}
	runner.FiltersQueue <- common.MapStr{"foo": "bar"}

	res := <-output
	assert.Equal(t, common.MapStr{"hello": "world"}, res)

	res = <-output
	assert.Equal(t, common.MapStr{"foo": "bar"}, res)
}

func TestLoadConfiguredFilters(t *testing.T) {
	loadPlugins()

	type o struct {
		Name string
		Type filters.Filter
	}

	type io struct {
		Input  map[string]interface{}
		Output []o
	}

	tests := []io{
		// should find configuration by types
		io{
			Input: map[string]interface{}{
				"filters": []interface{}{"nop1", "nop2"},
				"nop1": map[interface{}]interface{}{
					"type": "nop",
				},
				"nop2": map[interface{}]interface{}{
					"type": "nop",
				},
			},
			Output: []o{
				o{
					Name: "nop1",
					Type: filters.NopFilter,
				},
				o{
					Name: "nop2",
					Type: filters.NopFilter,
				},
			},
		},
		// should work with implicit configuration by name
		io{
			Input: map[string]interface{}{
				"filters": []interface{}{"nop", "sample1"},
				"sample1": map[interface{}]interface{}{
					"type": "nop",
				},
			},
			Output: []o{
				o{
					Name: "nop",
					Type: filters.NopFilter,
				},
				o{
					Name: "sample1",
					Type: filters.NopFilter,
				},
			},
		},
	}

	for _, test := range tests {
		res, err := LoadConfiguredFilters(test.Input)
		assert.Nil(t, err)

		res_o := []o{}
		for _, r := range res {
			res_o = append(res_o, o{Name: r.String(), Type: r.Type()})
		}

		assert.Equal(t, test.Output, res_o)
	}
}

func TestLoadConfiguredFiltersNegative(t *testing.T) {
	loadPlugins()

	type io struct {
		Input map[string]interface{}
		Err   string
	}

	tests := []io{
		io{
			Input: map[string]interface{}{
				"filters": []interface{}{"nop1", "nop2"},
				"nop1": map[interface{}]interface{}{
					"type": "nop",
				},
			},
			Err: "No such filter type and no corresponding configuration: nop2",
		},
		io{
			Input: map[string]interface{}{
				"filters": []interface{}{"nop1", "nop"},
				"nop1": map[interface{}]interface{}{
					"hype": "nop",
				},
			},
			Err: "Couldn't get type for filter: nop1",
		},
		io{
			Input: map[string]interface{}{
				"filters": []interface{}{"nop1", "nop"},
				"nop1": map[interface{}]interface{}{
					"type": 1,
				},
			},
			Err: "Couldn't get type for filter: nop1",
		},
	}

	for _, test := range tests {
		_, err := LoadConfiguredFilters(test.Input)
		assert.NotNil(t, err)
		assert.Equal(t, test.Err, err.Error())
	}
}
