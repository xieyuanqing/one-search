export const providerOptions = [
  { label: 'Brave Search', value: 'brave' },
  { label: 'Tavily', value: 'tavily' },
  { label: 'Exa', value: 'exa' },
  { label: 'Grok (AI Search)', value: 'grok' }
]

export const visibleProviders = new Set(providerOptions.map((item) => item.value))
export const defaultProviders = ['brave', 'grok']

export function providerLabel(provider: string) {
  return providerOptions.find((item) => item.value === provider)?.label || provider
}
