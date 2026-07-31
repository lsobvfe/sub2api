<script setup lang="ts">
import { computed, onUnmounted, ref, watch } from 'vue'
import { useIntervalFn } from '@vueuse/core'
import { useI18n } from 'vue-i18n'
import { opsAPI, type OpenAIStreamHoldSnapshot, type OpenAIStreamHoldState } from '@/api/admin/ops'
import Icon from '@/components/icons/Icon.vue'

interface Props {
  platformFilter?: string
  groupIdFilter?: number | null
  refreshToken: number
}

const props = withDefaults(defineProps<Props>(), {
  platformFilter: '',
  groupIdFilter: null
})

const STREAM_HOLD_REFRESH_INTERVAL_MS = 5000

const { t } = useI18n()
const loading = ref(false)
const errorMessage = ref('')
const snapshot = ref<OpenAIStreamHoldSnapshot | null>(null)
const clock = ref(Date.now())
let requestController: AbortController | null = null
let requestSequence = 0

const summary = computed(() => snapshot.value?.summary ?? {
  active_count: 0,
  holding_count: 0,
  retrying_count: 0,
  oldest_held_ms: 0,
  average_held_ms: 0,
  by_reason: {}
})

const holds = computed(() => snapshot.value?.holds ?? [])

function isCanceledRequest(err: unknown): boolean {
  return (
    !!err &&
    typeof err === 'object' &&
    'code' in err &&
    (err as Record<string, unknown>).code === 'ERR_CANCELED'
  )
}

async function loadData() {
  requestController?.abort()
  const controller = new AbortController()
  requestController = controller
  const sequence = ++requestSequence
  loading.value = true
  errorMessage.value = ''
  try {
    const data = await opsAPI.getActiveStreamHolds(
      props.platformFilter,
      props.groupIdFilter,
      undefined,
      { signal: controller.signal }
    )
    if (sequence !== requestSequence) return
    snapshot.value = data
    clock.value = Date.now()
  } catch (err: any) {
    if (isCanceledRequest(err) || sequence !== requestSequence) return
    console.error('[OpsStreamHoldsCard] Failed to load active stream holds', err)
    errorMessage.value = err?.response?.data?.message || t('admin.ops.streamHolds.loadFailed')
  } finally {
    if (sequence === requestSequence) {
      loading.value = false
    }
  }
}

function formatDuration(milliseconds: number): string {
  const totalSeconds = Math.max(0, Math.floor(milliseconds / 1000))
  if (totalSeconds < 60) return `${totalSeconds}s`
  const minutes = Math.floor(totalSeconds / 60)
  const seconds = totalSeconds % 60
  if (minutes < 60) return `${minutes}m ${seconds}s`
  const hours = Math.floor(minutes / 60)
  const remainingMinutes = minutes % 60
  return `${hours}h ${remainingMinutes}m`
}

function heldFor(hold: OpenAIStreamHoldState): string {
  const heldSince = Date.parse(hold.held_since)
  if (!Number.isFinite(heldSince)) return '0s'
  return formatDuration(clock.value - heldSince)
}

function formatTimestamp(value?: string | null): string {
  if (!value) return '-'
  const parsed = new Date(value)
  if (Number.isNaN(parsed.getTime())) return '-'
  return parsed.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' })
}

function phaseClass(phase: OpenAIStreamHoldState['phase']): string {
  return phase === 'holding'
    ? 'bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-300'
    : 'bg-blue-100 text-blue-700 dark:bg-blue-900/30 dark:text-blue-300'
}

function reasonLabel(reason: string): string {
  const key = `admin.ops.streamHolds.reasons.${reason}`
  const translated = t(key)
  return translated === key ? reason : translated
}

watch(
  () => [props.platformFilter, props.groupIdFilter, props.refreshToken] as const,
  () => {
    loadData()
  },
  { immediate: true }
)

useIntervalFn(() => {
  clock.value = Date.now()
}, 1000)

useIntervalFn(() => {
  loadData()
}, STREAM_HOLD_REFRESH_INTERVAL_MS)

onUnmounted(() => {
  requestSequence++
  requestController?.abort()
  requestController = null
})
</script>

<template>
  <section class="overflow-hidden rounded-2xl bg-white shadow-sm ring-1 ring-gray-900/5 dark:bg-dark-800 dark:ring-dark-700">
    <header class="flex flex-wrap items-center justify-between gap-3 border-b border-gray-100 px-5 py-4 dark:border-dark-700">
      <div class="flex min-w-0 items-center gap-3">
        <span class="flex h-9 w-9 shrink-0 items-center justify-center rounded-lg bg-amber-50 text-amber-600 dark:bg-amber-900/20 dark:text-amber-300">
          <Icon name="clock" size="sm" />
        </span>
        <div class="min-w-0">
          <h3 class="truncate text-sm font-bold text-gray-900 dark:text-white">
            {{ t('admin.ops.streamHolds.title') }}
          </h3>
          <p class="mt-0.5 text-xs text-gray-500 dark:text-gray-400">
            {{ snapshot?.enabled ? t('admin.ops.streamHolds.enabled') : t('admin.ops.streamHolds.disabled') }}
          </p>
        </div>
      </div>
      <button
        type="button"
        class="flex h-9 w-9 items-center justify-center rounded-lg text-gray-500 transition-colors hover:bg-gray-100 hover:text-gray-800 disabled:cursor-not-allowed disabled:opacity-50 dark:text-gray-400 dark:hover:bg-dark-700 dark:hover:text-gray-100"
        :disabled="loading"
        :title="t('common.refresh')"
        @click="loadData"
      >
        <Icon name="refresh" size="sm" :class="loading ? 'animate-spin' : ''" />
      </button>
    </header>

    <div class="grid grid-cols-2 divide-x divide-y divide-gray-100 border-b border-gray-100 sm:grid-cols-4 sm:divide-y-0 dark:divide-dark-700 dark:border-dark-700">
      <div class="px-5 py-3">
        <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.ops.streamHolds.active') }}</p>
        <p class="mt-1 text-xl font-bold text-gray-900 dark:text-white">{{ summary.active_count }}</p>
      </div>
      <div class="px-5 py-3">
        <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.ops.streamHolds.oldest') }}</p>
        <p class="mt-1 text-xl font-bold text-amber-600 dark:text-amber-300">{{ formatDuration(summary.oldest_held_ms) }}</p>
      </div>
      <div class="px-5 py-3">
        <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.ops.streamHolds.holding') }}</p>
        <p class="mt-1 text-xl font-bold text-gray-900 dark:text-white">{{ summary.holding_count }}</p>
      </div>
      <div class="px-5 py-3">
        <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.ops.streamHolds.retrying') }}</p>
        <p class="mt-1 text-xl font-bold text-blue-600 dark:text-blue-300">{{ summary.retrying_count }}</p>
      </div>
    </div>

    <div v-if="errorMessage" class="border-b border-red-100 bg-red-50 px-5 py-3 text-sm text-red-600 dark:border-red-900/30 dark:bg-red-900/20 dark:text-red-300">
      {{ errorMessage }}
    </div>

    <div v-if="holds.length === 0" class="px-5 py-10 text-center text-sm text-gray-500 dark:text-gray-400">
      {{ loading ? t('admin.ops.loadingText') : t('admin.ops.streamHolds.empty') }}
    </div>

    <div v-else class="overflow-x-auto">
      <table class="min-w-full divide-y divide-gray-100 text-left text-xs dark:divide-dark-700">
        <thead class="bg-gray-50 text-gray-500 dark:bg-dark-900 dark:text-gray-400">
          <tr>
            <th class="whitespace-nowrap px-5 py-3 font-semibold">{{ t('admin.ops.streamHolds.elapsed') }}</th>
            <th class="whitespace-nowrap px-4 py-3 font-semibold">{{ t('admin.ops.streamHolds.state') }}</th>
            <th class="whitespace-nowrap px-4 py-3 font-semibold">{{ t('admin.ops.streamHolds.request') }}</th>
            <th class="whitespace-nowrap px-4 py-3 font-semibold">{{ t('admin.ops.streamHolds.routing') }}</th>
            <th class="whitespace-nowrap px-4 py-3 font-semibold">{{ t('admin.ops.streamHolds.retry') }}</th>
            <th class="whitespace-nowrap px-5 py-3 text-right font-semibold">{{ t('admin.ops.streamHolds.updated') }}</th>
          </tr>
        </thead>
        <tbody class="divide-y divide-gray-100 dark:divide-dark-700">
          <tr v-for="hold in holds" :key="hold.client_request_id || hold.request_id" class="align-top text-gray-700 dark:text-gray-200">
            <td class="whitespace-nowrap px-5 py-3.5">
              <span class="font-mono text-sm font-semibold text-gray-900 dark:text-white">{{ heldFor(hold) }}</span>
            </td>
            <td class="px-4 py-3.5">
              <span class="inline-flex rounded-md px-2 py-1 font-medium" :class="phaseClass(hold.phase)">
                {{ t(`admin.ops.streamHolds.phases.${hold.phase}`) }}
              </span>
              <p class="mt-1.5 max-w-[240px] break-words text-gray-500 dark:text-gray-400">{{ reasonLabel(hold.reason) }}</p>
              <p v-if="hold.last_upstream_status_code" class="mt-1 font-mono text-red-500">
                HTTP {{ hold.last_upstream_status_code }}
              </p>
            </td>
            <td class="px-4 py-3.5">
              <p class="max-w-[260px] truncate font-medium text-gray-900 dark:text-white">{{ hold.model }}</p>
              <p class="mt-1 max-w-[260px] truncate font-mono text-gray-500 dark:text-gray-400" :title="hold.request_id">
                {{ hold.request_id }}
              </p>
              <p v-if="hold.last_error" class="mt-1 max-w-[320px] truncate text-red-500" :title="hold.last_error">{{ hold.last_error }}</p>
            </td>
            <td class="whitespace-nowrap px-4 py-3.5">
              <p>{{ hold.platform || '-' }}</p>
              <p class="mt-1 text-gray-500 dark:text-gray-400">
                {{ hold.account_id ? `#${hold.account_id}` : '-' }}
                <span v-if="hold.group_id"> · G#{{ hold.group_id }}</span>
              </p>
            </td>
            <td class="whitespace-nowrap px-4 py-3.5">
              <p>{{ t('admin.ops.streamHolds.cycle', { count: hold.hold_cycle }) }}</p>
              <p class="mt-1 text-gray-500 dark:text-gray-400">
                {{ hold.next_retry_at ? formatTimestamp(hold.next_retry_at) : t('admin.ops.streamHolds.upstreamAttempt') }}
              </p>
            </td>
            <td class="whitespace-nowrap px-5 py-3.5 text-right text-gray-500 dark:text-gray-400">
              {{ formatTimestamp(hold.updated_at) }}
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </section>
</template>
