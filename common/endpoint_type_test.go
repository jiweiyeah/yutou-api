package common

import (
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
)

func TestKiteDelayedSupportsClaudeAndOpenAIEndpoints(t *testing.T) {
	endpointTypes := GetEndpointTypesByChannelType(constant.ChannelTypeKiteDelayed, "glm-5.2")

	assert.Equal(t, []constant.EndpointType{
		constant.EndpointTypeAnthropic,
		constant.EndpointTypeOpenAI,
	}, endpointTypes)
}
