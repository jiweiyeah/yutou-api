import { expect, test } from 'vitest'

import { channelSchema } from '../types'
import {
  channelFormSchema,
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
  expect(defaults.response_model_name).toBe(false)

  const created = transformFormDataToCreatePayload({
    ...defaults,
    response_model_name: true,
  }).channel
  const reopened = transformChannelToFormDefaults({ ...channel, ...created })
  expect(reopened.response_model_name).toBe(true)
  expect(reopened.thinking_to_content).toBe(true)

  const saved = transformFormDataToUpdatePayload(
    { ...reopened, response_model_name: false },
    channel.id
  )
  const disabled = transformChannelToFormDefaults({ ...channel, ...saved })
  expect(disabled.response_model_name).toBe(false)
  expect(disabled.thinking_to_content).toBe(true)
})

const ATTRIA_REWRITE_RULE = {
  match: 'is not supported by TokenPlan',
  status_code: 413,
  message: '请求体过大：本次 {body_kb} KB',
  code: 'request_too_large',
  skip_retry: true,
}

function atriaRewriteChannel() {
  return channelSchema.parse({
    id: 10864,
    type: 58,
    name: 'atria-asi',
    key: '',
    models: 'deepseek/deepseek-v4-flash',
    group: 'default',
    status: 1,
    created_time: 0,
    test_time: 0,
    response_time: 0,
    balance_updated_time: 0,
    setting: '',
    settings: JSON.stringify({
      advanced_custom: {
        advanced_routes: [
          {
            incoming_path: '/v1/chat/completions',
            upstream_path: '/v1/chat/completions',
            converter: 'none',
          },
        ],
      },
      error_rewrite: [ATTRIA_REWRITE_RULE],
    }),
  })
}

test('error rewrite rules survive channel editing round trips', () => {
  const channel = atriaRewriteChannel()

  const defaults = transformChannelToFormDefaults(channel)
  expect(defaults.error_rewrite).toEqual([ATTRIA_REWRITE_RULE])

  const saved = transformFormDataToUpdatePayload(defaults, channel.id)
  const settings = JSON.parse(String(saved.settings))
  expect(settings.error_rewrite).toEqual([ATTRIA_REWRITE_RULE])
  // Settings written outside the UI must not be dropped by a UI save.
  expect(settings.advanced_custom.advanced_routes.length).toBe(1)

  const reopened = transformChannelToFormDefaults({ ...channel, ...saved })
  expect(reopened.error_rewrite).toEqual([ATTRIA_REWRITE_RULE])
})

test('removing every rewrite rule deletes the settings key', () => {
  const channel = atriaRewriteChannel()
  const defaults = transformChannelToFormDefaults(channel)

  const saved = transformFormDataToUpdatePayload(
    { ...defaults, error_rewrite: [] },
    channel.id
  )
  const settings = JSON.parse(String(saved.settings))
  expect('error_rewrite' in settings).toBe(false)
  expect(settings.advanced_custom.advanced_routes.length).toBe(1)
})

test('blank and unset rewrite rule fields are not persisted', () => {
  const channel = atriaRewriteChannel()
  const defaults = transformChannelToFormDefaults(channel)

  const saved = transformFormDataToUpdatePayload(
    {
      ...defaults,
      error_rewrite: [
        { match: '  TokenPlan  ', status_code: 0, message: '', code: '' },
        { match: '   ' },
      ],
    },
    channel.id
  )
  const settings = JSON.parse(String(saved.settings))
  expect(settings.error_rewrite).toEqual([{ match: 'TokenPlan' }])
})

function errorRewriteIssues(formData: Record<string, unknown>): string[] {
  const result = channelFormSchema.safeParse(formData)
  if (result.success) {
    return []
  }
  return result.error.issues.map((issue) => issue.message)
}

test('schema rejects a rewrite rule without match text', () => {
  const channel = atriaRewriteChannel()
  const defaults = transformChannelToFormDefaults(channel)

  const messages = errorRewriteIssues({
    ...defaults,
    error_rewrite: [{ match: '   ', status_code: 413 }],
  })
  expect(
    messages.some((message) => message.includes('match text is required')),
    `expected a match text error, got ${JSON.stringify(messages)}`
  ).toBe(true)
})

test('schema rejects a rewrite rule with an out-of-range status code', () => {
  const channel = atriaRewriteChannel()
  const defaults = transformChannelToFormDefaults(channel)

  const messages = errorRewriteIssues({
    ...defaults,
    error_rewrite: [{ match: 'TokenPlan', status_code: 200 }],
  })
  expect(
    messages.some((message) =>
      message.includes('status code must be between 400 and 599')
    ),
    `expected a status code error, got ${JSON.stringify(messages)}`
  ).toBe(true)
})

test('schema rejects duplicate rewrite match text', () => {
  const channel = atriaRewriteChannel()
  const defaults = transformChannelToFormDefaults(channel)

  const messages = errorRewriteIssues({
    ...defaults,
    error_rewrite: [
      { match: 'TokenPlan', status_code: 413 },
      { match: 'tokenplan', status_code: 413 },
    ],
  })
  expect(
    messages.some((message) => message.includes('duplicates rule 1')),
    `expected a duplicate error, got ${JSON.stringify(messages)}`
  ).toBe(true)
})
