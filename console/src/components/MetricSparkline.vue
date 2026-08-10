<script setup lang="ts">
import { computed } from 'vue'

interface Props {
  data: number[]
  width?: number
  height?: number
  color?: string
  fillOpacity?: number
}

const props = withDefaults(defineProps<Props>(), {
  width: 120,
  height: 32,
  color: '#0ea5e9',
  fillOpacity: 0.15,
})

const linePath = computed(() => {
  const values = props.data
  if (values.length < 2) return ''

  const max = Math.max(...values)
  const min = Math.min(...values)
  const range = max - min || 1
  const padding = 1

  const stepX = (props.width - padding * 2) / (values.length - 1)

  return values
    .map((v, i) => {
      const x = padding + i * stepX
      const y = padding + (1 - (v - min) / range) * (props.height - padding * 2)
      return `${i === 0 ? 'M' : 'L'}${x.toFixed(1)},${y.toFixed(1)}`
    })
    .join(' ')
})

const fillPath = computed(() => {
  if (!linePath.value) return ''
  const padding = 1
  return `${linePath.value} L${(props.width - padding).toFixed(1)},${(props.height - padding).toFixed(1)} L${padding},${(props.height - padding).toFixed(1)} Z`
})
</script>

<template>
  <svg :width="width" :height="height" class="overflow-visible">
    <path
      v-if="fillPath"
      :d="fillPath"
      :fill="color"
      :fill-opacity="fillOpacity"
    />
    <path
      v-if="linePath"
      :d="linePath"
      fill="none"
      :stroke="color"
      stroke-width="1.5"
      stroke-linecap="round"
      stroke-linejoin="round"
    />
  </svg>
</template>
