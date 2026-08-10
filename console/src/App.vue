<script setup lang="ts">
import { RouterView } from 'vue-router'
import ToastContainer from '@/components/ToastContainer.vue'
import ModalContainer from '@/components/ModalContainer.vue'
</script>

<template>
  <RouterView />
  <ToastContainer />
  <ModalContainer />
</template>
