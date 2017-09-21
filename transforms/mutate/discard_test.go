package mutate

import (
	"testing"

	"github.com/qiniu/logkit/sender"
	"github.com/qiniu/logkit/transforms"
	"github.com/stretchr/testify/assert"
)

func TestDiscardTransformer(t *testing.T) {
	dis := &Discarder{
		Key: "myword",
	}
	data, err := dis.Transform([]sender.Data{{"myword": "hello x1 y2 x1nihao", "abc": "x1 y2"}, {"myword": "x1x.x.x11", "abc": "x1"}})
	assert.NoError(t, err)
	exp := []sender.Data{
		{"abc": "x1 y2"},
		{"abc": "x1"}}
	assert.Equal(t, exp, data)

	assert.Equal(t, dis.Stage(), transforms.StageAfterParser)
}
