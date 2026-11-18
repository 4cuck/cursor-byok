<script setup>
import { computed } from "vue";
import { defaultCursorContextWindow, formatContextWindowTokens } from "@/utils/cursorContextWindows";

// Context window picker for Cursor account models. The options come from Cursor's
// catalog via "Sync models"; the parent owns the draft and persistence.
const props = defineProps({
  windows: {
    type: Array,
    default: () => [],
  },
  modelValue: {
    type: Number,
    default: 0,
  },
});

const emit = defineEmits(["update:modelValue"]);

const defaultWindow = computed(() => defaultCursorContextWindow(props.windows));
const selectedWindow = computed(() =>
  props.windows.find((option) => option.tokens === props.modelValue) || defaultWindow.value,
);
</script>

<template>
  <div class="flex flex-col gap-1.5">
    <span class="text-sm text-[#d4d4d4]">Context window</span>

    <template v-if="windows.length">
      <div
        class="inline-flex w-fit flex-wrap rounded-[8px] border border-[#3f3f3f] bg-[#232323] p-0.5 text-[12px]"
        role="radiogroup"
        aria-label="Cursor context window"
      >
        <button
          v-for="option in windows"
          :key="option.tokens"
          type="button"
          role="radio"
          :aria-checked="selectedWindow?.tokens === option.tokens"
          :title="`${option.tokens.toLocaleString()} tokens`"
          class="center-row gap-1.5 rounded-[6px] px-2.5 py-1 font-medium transition-colors"
          :class="selectedWindow?.tokens === option.tokens
            ? 'bg-[#10AD5D]/25 text-[#6ee7a5]'
            : 'text-[#a3a3a3] hover:text-white'"
          @click="emit('update:modelValue', option.tokens)"
        >
          <span>{{ formatContextWindowTokens(option.tokens) }}</span>
          <span
            v-if="option.maxMode"
            class="rounded-[4px] border border-[#b45309]/60 bg-[#78350f]/40 px-1 text-[10px] font-semibold leading-4 text-[#fcd34d]"
          >MAX</span>
        </button>
      </div>
      <div class="text-xs leading-5 text-[#8f8f8f]">
        <template v-if="selectedWindow?.maxMode">
          Cursor serves {{ formatContextWindowTokens(selectedWindow.tokens) }} only in Max mode; fallback calls without an IDE context choice run as Max mode.
        </template>
        <template v-else>
          Fallback calls without an IDE context choice use Cursor's {{ formatContextWindowTokens(selectedWindow?.tokens) }} window.
        </template>
        All windows Cursor offers remain selectable in the Cursor IDE model picker (Context).
        <template v-if="defaultWindow && windows.length > 1">
          Cursor's default is {{ formatContextWindowTokens(defaultWindow.tokens) }}.
        </template>
      </div>
    </template>

    <div
      v-else
      class="text-xs leading-5 text-[#8f8f8f]"
    >
      <template v-if="modelValue > 0">
        {{ modelValue.toLocaleString() }} tokens.
      </template>
      Run "Sync models" on the Cursor account to load the context windows Cursor offers for this model.
    </div>
  </div>
</template>