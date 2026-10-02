/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { Plus, Trash2 } from 'lucide-react'
import { useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Switch } from '@/components/ui/switch'
import { Textarea } from '@/components/ui/textarea'

import type { ChannelErrorRewriteRule } from '../types'

type ErrorRewriteEditorProps = {
  value: ChannelErrorRewriteRule[]
  onChange: (value: ChannelErrorRewriteRule[]) => void
}

const EMPTY_RULE: ChannelErrorRewriteRule = {
  match: '',
  status_code: 0,
  message: '',
  code: '',
  skip_retry: false,
}

export function ErrorRewriteEditor(props: ErrorRewriteEditorProps) {
  const { t } = useTranslation()
  const rules = props.value ?? []

  // Rows are keyed by a stable id, not by index, so editing one rule never
  // remounts another row's inputs. Ids only need to live as long as the drawer.
  const nextIdRef = useRef(0)
  const [rowIds, setRowIds] = useState<string[]>([])
  useEffect(() => {
    setRowIds((prev) => {
      if (prev.length === rules.length) {
        return prev
      }
      const next = prev.slice(0, rules.length)
      while (next.length < rules.length) {
        nextIdRef.current += 1
        next.push(`rewrite-rule-${nextIdRef.current}`)
      }
      return next
    })
  }, [rules.length])

  const updateRule = (
    index: number,
    patch: Partial<ChannelErrorRewriteRule>
  ) => {
    props.onChange(
      rules.map((rule, current) =>
        current === index ? { ...rule, ...patch } : rule
      )
    )
  }

  const addRule = () => {
    props.onChange([...rules, { ...EMPTY_RULE }])
  }

  const removeRule = (index: number) => {
    props.onChange(rules.filter((_, current) => current !== index))
  }

  return (
    <div className='space-y-3'>
      {rules.length === 0 && (
        <p className='text-muted-foreground text-xs'>
          {t('No rewrite rules configured yet')}
        </p>
      )}

      {rules.map((rule, index) => (
        <div
          key={rowIds[index] ?? `rewrite-rule-pending-${index}`}
          className='border-border/60 space-y-3 rounded-lg border p-3'
        >
          <div className='flex items-center justify-between gap-2'>
            <Badge variant='secondary'>
              {t('Rule {{index}}', { index: index + 1 })}
            </Badge>
            <Button
              type='button'
              variant='ghost'
              size='sm'
              onClick={() => removeRule(index)}
            >
              <Trash2 className='mr-1 h-4 w-4' />
              {t('Remove rule')}
            </Button>
          </div>

          <div className='space-y-1'>
            <label className='text-[13px] font-medium'>
              {t('Match text')}
            </label>
            <Input
              value={rule.match ?? ''}
              placeholder='Atria-Dawn-Preview is not supported by TokenPlan'
              onChange={(event) =>
                updateRule(index, { match: event.target.value })
              }
            />
            <p className='text-muted-foreground text-xs'>
              {t('Case-insensitive substring of the upstream error message')}
            </p>
          </div>

          <div className='grid gap-3 sm:grid-cols-2'>
            <div className='space-y-1'>
              <label className='text-[13px] font-medium'>
                {t('Replacement status code')}
              </label>
              <Input
                type='number'
                min={400}
                max={599}
                placeholder='413'
                value={rule.status_code ? String(rule.status_code) : ''}
                onChange={(event) => {
                  const raw = event.target.value.trim()
                  updateRule(index, {
                    status_code: raw === '' ? 0 : Number(raw),
                  })
                }}
              />
            </div>
            <div className='space-y-1'>
              <label className='text-[13px] font-medium'>
                {t('Replacement error code')}
              </label>
              <Input
                value={rule.code ?? ''}
                placeholder='request_too_large'
                onChange={(event) =>
                  updateRule(index, { code: event.target.value })
                }
              />
            </div>
          </div>

          <div className='space-y-1'>
            <label className='text-[13px] font-medium'>
              {t('Message shown to the client')}
            </label>
            <Textarea
              rows={2}
              value={rule.message ?? ''}
              onChange={(event) =>
                updateRule(index, { message: event.target.value })
              }
            />
            <p className='text-muted-foreground text-xs'>
              {t(
                'Supported placeholders: {body_bytes}, {body_kb}, {model}, {upstream_message}'
              )}
            </p>
          </div>

          <div className='flex items-center justify-between gap-4 px-1'>
            <div className='space-y-0.5'>
              <div className='text-[13px] font-medium'>{t('Skip retry')}</div>
              <p className='text-muted-foreground text-xs'>
                {t('Stop retrying this error on other keys or channels')}
              </p>
            </div>
            <Switch
              checked={rule.skip_retry === true}
              onCheckedChange={(checked) =>
                updateRule(index, { skip_retry: checked })
              }
            />
          </div>

          <p className='text-muted-foreground text-xs'>
            {t('Leave empty to keep the upstream value')}
          </p>
        </div>
      ))}

      <Button type='button' variant='outline' size='sm' onClick={addRule}>
        <Plus className='mr-1 h-4 w-4' />
        {t('Add rule')}
      </Button>
    </div>
  )
}
