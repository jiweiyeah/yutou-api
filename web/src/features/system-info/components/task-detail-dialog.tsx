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
// ===== CUSTOM START: 任务运行详情弹框 =====
// 后端把每次运行的统计（result）、进度（state）和入参（payload）都返回了，
// 表格里只展示 error 时成功的任务详情列永远是空的，这里改成按需展开完整内容。
import { useTranslation } from 'react-i18next'

import { Dialog } from '@/components/dialog'

import type { SystemTask } from '@/features/system-settings/types'

import { SYSTEM_TASK_TYPE_LABEL, TYPE_DISPLAY_ID } from '../constants'

function formatDetailValue(value: unknown): string {
  if (value === null || value === undefined) return '-'
  if (typeof value === 'string') return value || '-'
  if (typeof value === 'number' || typeof value === 'boolean')
    return String(value)
  return JSON.stringify(value, null, 2)
}

function isPlainRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function DetailSection(props: { title: string; value: unknown }) {
  if (props.value === null || props.value === undefined) return null

  return (
    <div className='space-y-1.5'>
      <h4 className='text-xs font-medium'>{props.title}</h4>
      {isPlainRecord(props.value) ? (
        <dl className='divide-border grid grid-cols-1 divide-y rounded-md border text-xs'>
          {Object.entries(props.value).map(([key, entry]) => (
            <div
              key={key}
              className='grid grid-cols-[minmax(0,1fr)_minmax(0,1fr)] gap-3 px-3 py-1.5'
            >
              <dt className='text-muted-foreground truncate font-mono'>
                {key}
              </dt>
              <dd className='font-mono break-all whitespace-pre-wrap'>
                {formatDetailValue(entry)}
              </dd>
            </div>
          ))}
        </dl>
      ) : (
        <pre className='bg-muted/40 max-h-60 overflow-auto rounded-md border p-3 font-mono text-xs break-all whitespace-pre-wrap'>
          {formatDetailValue(props.value)}
        </pre>
      )}
    </div>
  )
}

export function TaskDetailDialog(props: {
  task: SystemTask | null
  onOpenChange: (open: boolean) => void
}) {
  const { t } = useTranslation()
  const task = props.task
  if (!task) return null

  const hasDetail =
    task.result != null ||
    task.state != null ||
    task.payload != null ||
    Boolean(task.error)

  return (
    <Dialog
      open
      onOpenChange={props.onOpenChange}
      title={t(SYSTEM_TASK_TYPE_LABEL[task.type] ?? task.type)}
      description={task.task_id}
      contentClassName='sm:max-w-3xl'
    >
      <div className='space-y-4 overflow-y-auto'>
        <DetailSection
          title={t('Overview')}
          value={{
            type: TYPE_DISPLAY_ID[task.type] ?? task.type,
            status: task.status,
            executor: task.locked_by || '-',
          }}
        />
        <DetailSection title={t('Result')} value={task.result} />
        <DetailSection title={t('Progress')} value={task.state} />
        <DetailSection title={t('Parameters')} value={task.payload} />
        {task.error ? (
          <div className='space-y-1.5'>
            <h4 className='text-xs font-medium'>{t('Error')}</h4>
            <pre className='text-destructive bg-destructive/5 border-destructive/20 max-h-60 overflow-auto rounded-md border p-3 font-mono text-xs break-all whitespace-pre-wrap'>
              {task.error}
            </pre>
          </div>
        ) : null}
        {hasDetail ? null : (
          <p className='text-muted-foreground text-sm'>
            {t('This task did not record any detail.')}
          </p>
        )}
      </div>
    </Dialog>
  )
}

// ===== CUSTOM END =====
