<template>
  <div>
    <v-text-field
      ref="searchBoxField"
      autofocus
      hide-details
      flat
      prepend-inner-icon="mdi-magnify"
      :value="searchBox"
      @input="onInput"
      @click.stop
      @keyup.enter="onEnter"
      @keydown.up.prevent="arrowUp"
      @keydown.down.prevent="arrowDown"
      @keydown.tab.exact.prevent.stop="onTab"
      @keydown.esc.exact="suggestionMenuOpen = false"
    >
      <template #append>
        <v-menu offset-y right bottom>
          <template #activator="{ on, attrs }">
            <v-btn small icon v-bind="attrs" v-on="on"
              ><v-icon>mdi-dots-vertical</v-icon></v-btn
            >
          </template>
          <v-list dense>
            <v-list-item link @click="search('search')">
              <v-list-item-icon><v-icon>mdi-magnify</v-icon></v-list-item-icon>
              <v-list-item-title>Search</v-list-item-title>
            </v-list-item>
            <v-list-item link @click="search('graph')">
              <v-list-item-icon><v-icon>mdi-finance</v-icon></v-list-item-icon>
              <v-list-item-title>Graph</v-list-item-title>
            </v-list-item>
            <v-list-item link @click="createTag('service', searchBox)">
              <v-list-item-icon
                ><v-icon>mdi-cloud-outline</v-icon></v-list-item-icon
              >
              <v-list-item-title>Save as Service</v-list-item-title>
            </v-list-item>
            <v-list-item link @click="createTag('tag', searchBox)">
              <v-list-item-icon
                ><v-icon>mdi-tag-multiple-outline</v-icon></v-list-item-icon
              >
              <v-list-item-title>Save as Tag</v-list-item-title>
            </v-list-item>
          </v-list>
        </v-menu>
      </template>
    </v-text-field>
    <v-menu
      ref="suggestionMenu"
      v-model="suggestionMenuOpen"
      :position-x="suggestionMenuPosX"
      :position-y="suggestionMenuPosY"
      absolute
      dense
    >
      <v-list>
        <v-list-item-group
          :value="suggestionSelectedIndex"
          color="primary"
          mandatory
        >
          <v-list-item
            v-for="(item, index) in suggestionItems"
            :key="index"
            active-class="font-white"
            :style="{ backgroundColor: suggestionColor(suggestionType, item) }"
            @click="applySuggestion(index)"
          >
            <v-list-item-title>{{ item }}</v-list-item-title>
          </v-list-item>
        </v-list-item-group>
      </v-list>
    </v-menu>
  </div>
</template>

<script lang="ts" setup>
import { EventBus } from "./EventBus";
import { addSearch, getTermAt } from "./searchHistory";
import suggest from "@/parser/suggest";
import { computed, ref, onMounted, onBeforeUnmount, watch } from "vue";
import { useRoute, useRouter } from "vue-router/composables";
import { useStore } from "@/store";
import { tagNameForURI } from "@/filters/tagNameForURI";
import { VTextField } from "vuetify/lib";

const store = useStore();
const route = useRoute();
const router = useRouter();
const searchBoxField = ref<InstanceType<typeof VTextField> | null>(null);
const searchBox = ref<string>(route.query.q as string);
const historyIndex = ref(-1);
const pendingSearch = ref("");
const typingDelay = ref<number | null>(null);
const suggestionItems = ref<string[]>([]);
const suggestionStart = ref(0);
const suggestionEnd = ref(0);
const suggestionType = ref("tag");
const suggestionSelectedIndex = ref(0);
const suggestionMenuOpen = ref(false);
const suggestionMenuPosX = ref(0);
const suggestionMenuPosY = ref(0);
const tagColors = computed(() => {
  const tags: { [key: string]: { [key: string]: string } } = {};
  if (store.state.tags == null) return tags;
  store.state.tags.forEach((tag) => {
    const type = tag.Name.split("/", 1)[0];
    const name = tag.Name.substr(type.length + 1);
    if (!(type in tags)) {
      tags[type] = {};
    }
    tags[type][name] = tag.Color;
  });
  return tags;
});

EventBus.on("setSearchTerm", setSearchTerm);

watch(
  route,
  () => {
    setSearchBox(route.query.q as string);
  },
  { immediate: true }
);
watch(
  suggestionItems,
  () => {
    suggestionMenuOpen.value = suggestionItems.value.length > 0;
    if (suggestionMenuOpen.value) {
      suggestionSelectedIndex.value = 0;
      const cursorIndex =
        searchBoxField.value?.$el.querySelector("input")?.selectionStart ??
        null;
      if (cursorIndex === null) return;
      const fontWidth = 7.05; // @TODO: Calculate the absolute cursor position correctly
      suggestionMenuPosX.value =
        cursorIndex * fontWidth +
        (searchBoxField.value?.$el.getBoundingClientRect().left ?? 0);
    }
  },
  { immediate: true }
);

onMounted(() => {
  store.dispatch("updateConverters").catch((err: string) => {
    EventBus.emit("showError", `Failed to update converters: ${err}`);
  });
  const keyListener = (e: KeyboardEvent) => {
    if (e.target === null || !(e.target instanceof Element)) return;
    if (["input", "textarea"].includes(e.target.tagName.toLowerCase())) return;
    if (e.key != "/") return;
    e.preventDefault();
    searchBoxField.value?.$el.querySelector("input")?.focus();
  };
  document.body.addEventListener("keydown", keyListener);
  onBeforeUnmount(() => {
    document.body.removeEventListener("keydown", keyListener);
  });
  suggestionMenuPosY.value =
    searchBoxField.value?.$el.getBoundingClientRect().bottom ?? 0;
});

function onTab() {
  if (suggestionMenuOpen.value) {
    applySuggestion();
  } else {
    startSuggestionSearch();
  }
}

function onInput(updatedText: string) {
  historyIndex.value = -1;
  setSearchBox(updatedText);
  startSuggestionSearch();
}

function onEnter() {
  if (suggestionMenuOpen.value) {
    applySuggestion();
  } else {
    search(null);
  }
}

function setSearchBox(value: string) {
  searchBox.value = value;
  abortSuggestionSearch();
}

function setSearchTerm(searchTerm: string) {
  setSearchBox(searchTerm);
}

function applySuggestion(index: number | null = null) {
  let replace = suggestionItems.value[index ?? suggestionSelectedIndex.value];
  if (replace === null || searchBox.value === null) {
    return;
  }
  replace = tagNameForURI(replace);
  const prefix = searchBox.value.substring(0, suggestionStart.value);
  const suffix = searchBox.value.substring(suggestionEnd.value);
  searchBox.value = prefix + replace + suffix;
  suggestionMenuOpen.value = false;
}

type SuggestionResults = {
  suggestions: string[];
  start: number;
  end: number;
  type: string;
};

function startSuggestionSearch() {
  const val = searchBox.value;
  typingDelay.value = setTimeout(() => {
    const cursorPosition =
      searchBoxField.value?.$el.querySelector("input")?.selectionStart ?? 0;
    const suggestionResult: SuggestionResults = suggest(
      val,
      cursorPosition,
      store.getters.groupedTags,
      store.state.converters
    ) as SuggestionResults; // trust me bro
    suggestionItems.value = suggestionResult.suggestions;
    suggestionStart.value = suggestionResult.start;
    suggestionEnd.value = suggestionResult.end;
    suggestionType.value = suggestionResult.type;
  }, 200);
}

function abortSuggestionSearch() {
  if (typingDelay.value) {
    clearTimeout(typingDelay.value);
    suggestionItems.value = [];
    typingDelay.value = null;
  }
}

function suggestionColor(type: string, item: string) {
  if (type === "data") {
    return "#ffffff";
  }
  return tagColors.value[type][item];
}

function arrowUp() {
  if (suggestionMenuOpen.value) {
    menuUp();
  } else {
    historyUp();
  }
}

function arrowDown() {
  if (suggestionMenuOpen.value) {
    menuDown();
  } else {
    historyDown();
  }
}

function menuDown() {
  selectSuggestionIndex(suggestionSelectedIndex.value + 1);
}

function menuUp() {
  selectSuggestionIndex(suggestionSelectedIndex.value - 1);
}

function selectSuggestionIndex(index: number) {
  suggestionSelectedIndex.value = Math.min(
    Math.max(index, 0),
    suggestionItems.value.length - 1
  );
}

function historyUp() {
  if (historyIndex.value === -1) {
    pendingSearch.value = searchBox.value;
  }
  let term = getTermAt(historyIndex.value + 1);
  if (term == null) {
    return;
  }
  historyIndex.value++;
  if (pendingSearch.value === term) {
    historyUp();
    return;
  }
  setSearchBox(term);
}

function historyDown() {
  if (historyIndex.value === -1) {
    return;
  }
  historyIndex.value--;
  setSearchBox(
    historyIndex.value === -1
      ? pendingSearch.value
      : getTermAt(historyIndex.value)
  );
}

function search(type: string | null) {
  let q: typeof route.query = {};
  if (!type) {
    type = route.name == "graph" ? "graph" : "search";
    if (type == "graph")
      q = JSON.parse(JSON.stringify(route.query)) as typeof route.query;
  }
  q.q = searchBox.value;
  addSearch(searchBox.value);
  historyIndex.value = -1;
  void router.push({
    name: type,
    query: q,
  });
}

function createTag(tagType: string, tagQuery: string) {
  EventBus.emit("showCreateTagDialog", tagType, tagQuery, []);
}
</script>

<style scoped>
.font-white {
  color: black;
  font-weight: bold;
}
</style>
