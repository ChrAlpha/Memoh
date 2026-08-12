<template>
  <div class="flex flex-wrap gap-3">
    <label
      v-for="option in COMPATIBILITY_OPTIONS"
      :key="option.value"
      class="flex items-center gap-1.5 text-xs"
    >
      <Checkbox
        :model-value="modelValue.includes(option.value)"
        @update:model-value="(checked: boolean | 'indeterminate') => toggle(option.value, checked === true)"
      />
      {{ $t(`models.compatibility.${option.value}`) }}
    </label>
  </div>
</template>

<script setup lang="ts">
import { Checkbox } from '@felinic/ui'
import { COMPATIBILITY_OPTIONS } from '@/constants/compatibilities'

const modelValue = defineModel<string[]>({ default: () => ['tool-call'] })

function toggle(capability: string, checked: boolean) {
  modelValue.value = checked
    ? [...new Set([...modelValue.value, capability])]
    : modelValue.value.filter(value => value !== capability)
}
</script>
