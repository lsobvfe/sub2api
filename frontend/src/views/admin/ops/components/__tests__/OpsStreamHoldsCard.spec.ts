import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import OpsStreamHoldsCard from '../OpsStreamHoldsCard.vue'

const mockGetActiveStreamHolds = vi.fn()

vi.mock('@/api/admin/ops', () => ({
  opsAPI: {
    getActiveStreamHolds: (...args: any[]) => mockGetActiveStreamHolds(...args)
  }
}))

vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, any>) => (
        params?.count === undefined ? key : `${key}:${params.count}`
      )
    })
  }
})

const sampleSnapshot = {
  enabled: true,
  holds: [
    {
      request_id: 'request-hold-1',
      client_request_id: 'client-request-hold-1',
      user_id: 10,
      api_key_id: 11,
      group_id: 12,
      account_id: 13,
      platform: 'openai',
      model: 'gpt-5.1',
      request_path: '/v1/responses',
      phase: 'holding' as const,
      reason: 'upstream_unavailable',
      hold_cycle: 2,
      request_started_at: '2026-07-31T12:00:00Z',
      held_since: '2026-07-31T12:00:01Z',
      updated_at: '2026-07-31T12:00:02Z',
      retry_delay_ms: 30000,
      next_retry_at: '2026-07-31T12:00:32Z',
      last_upstream_status_code: 502,
      last_error: 'upstream error: 502'
    }
  ],
  summary: {
    active_count: 1,
    holding_count: 1,
    retrying_count: 0,
    oldest_held_ms: 5000,
    average_held_ms: 5000,
    by_reason: { upstream_unavailable: 1 }
  },
  timestamp: '2026-07-31T12:00:06Z'
}

describe('OpsStreamHoldsCard', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mockGetActiveStreamHolds.mockResolvedValue(sampleSnapshot)
  })

  it('loads active holds with dashboard filters and renders live context', async () => {
    const wrapper = mount(OpsStreamHoldsCard, {
      props: {
        platformFilter: 'openai',
        groupIdFilter: 12,
        refreshToken: 0
      },
      global: {
        stubs: {
          Icon: { template: '<span />' }
        }
      }
    })

    await flushPromises()

    expect(mockGetActiveStreamHolds).toHaveBeenCalledWith(
      'openai',
      12,
      undefined,
      expect.objectContaining({ signal: expect.any(AbortSignal) })
    )
    expect(wrapper.text()).toContain('request-hold-1')
    expect(wrapper.text()).toContain('gpt-5.1')
    expect(wrapper.text()).toContain('HTTP 502')
    expect(wrapper.text()).toContain('#13')
  })

  it('reloads when the dashboard refresh token changes', async () => {
    const wrapper = mount(OpsStreamHoldsCard, {
      props: {
        platformFilter: '',
        groupIdFilter: null,
        refreshToken: 0
      },
      global: {
        stubs: {
          Icon: { template: '<span />' }
        }
      }
    })
    await flushPromises()
    mockGetActiveStreamHolds.mockClear()

    await wrapper.setProps({ refreshToken: 1 })
    await flushPromises()

    expect(mockGetActiveStreamHolds).toHaveBeenCalledTimes(1)
  })

  it('cancels an in-flight refresh when unmounted', async () => {
    let requestSignal: AbortSignal | undefined
    mockGetActiveStreamHolds.mockImplementation(
      (_platform, _groupId, _accountId, options) => {
        requestSignal = options.signal
        return new Promise(() => {})
      }
    )
    const wrapper = mount(OpsStreamHoldsCard, {
      props: {
        platformFilter: '',
        groupIdFilter: null,
        refreshToken: 0
      },
      global: {
        stubs: {
          Icon: { template: '<span />' }
        }
      }
    })
    await flushPromises()

    wrapper.unmount()

    expect(requestSignal?.aborted).toBe(true)
  })
})
