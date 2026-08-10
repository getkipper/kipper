import pluginVue from 'eslint-plugin-vue'
import { defineConfigWithVueTs, vueTsConfigs } from '@vue/eslint-config-typescript'

export default defineConfigWithVueTs(
  { ignores: ['dist/', 'node_modules/', 'coverage/'] },
  pluginVue.configs['flat/essential'],
  vueTsConfigs.recommended,
  {
    rules: {
      '@typescript-eslint/no-explicit-any': 'error',
      // no-explicit-any only sees <script>; these cover template expressions.
      // The template visitor skips TS type nodes, so `any` is matched through
      // the annotation attribute rather than a TSAnyKeyword selector.
      'vue/no-restricted-syntax': [
        'error',
        {
          selector: '[typeAnnotation.typeAnnotation.type="TSAnyKeyword"]',
          message: 'Unexpected any. Specify a different type.',
        },
        {
          selector: 'TSAsExpression[typeAnnotation.type="TSAnyKeyword"]',
          message: 'Unexpected any. Specify a different type.',
        },
      ],
    },
  },
  {
    // Routed pages are single-word by convention; the rule guards
    // against clashes with HTML elements, which view names never hit.
    files: ['src/views/**/*.vue'],
    rules: {
      'vue/multi-word-component-names': 'off',
    },
  },
)
