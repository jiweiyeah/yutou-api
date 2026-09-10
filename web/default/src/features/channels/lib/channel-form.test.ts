import assert from 'node:assert/strict'
import { test } from 'node:test'

import { channelSchema } from '../types'
import {
  transformChannelToFormDefaults,
  transformFormDataToCreatePayload,
  transformFormDataToUpdatePayload,
} from './channel-form'

test('response model setting survives channel creation, editing and disabling', () => {
  const channel = channelSchema.parse({
    id: 1,
    type: 58,
    name: 'test',
    key: '',
    models: 'public-model',
    group: 'default',
    status: 1,
    created_time: 0,
    test_time: 0,
    response_time: 0,
    balance_updated_time: 0,
    setting: '{"thinking_to_content":true}',
  })
  const defaults = transformChannelToFormDefaults(channel)
  assert.equal(defaults.response_model_name, false)

  const created = transformFormDataToCreatePayload({
    ...defaults,
    response_model_name: true,
  }).channel
  const reopened = transformChannelToFormDefaults({ ...channel, ...created })
  assert.equal(reopened.response_model_name, true)
  assert.equal(reopened.thinking_to_content, true)

  const saved = transformFormDataToUpdatePayload(
    { ...reopened, response_model_name: false },
    channel.id
  )
  const disabled = transformChannelToFormDefaults({ ...channel, ...saved })
  assert.equal(disabled.response_model_name, false)
  assert.equal(disabled.thinking_to_content, true)
})
