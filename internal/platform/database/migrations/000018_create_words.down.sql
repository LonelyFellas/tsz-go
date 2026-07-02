-- Children first: word_sense_relations references both words and word_senses,
-- so it must go before either.
DROP TABLE IF EXISTS word_sense_relations;
DROP TABLE IF EXISTS word_sense_sentences;
DROP TABLE IF EXISTS word_sense_definitions;
DROP TABLE IF EXISTS word_senses;
DROP TABLE IF EXISTS word_grammar_structure_variants;
DROP TABLE IF EXISTS word_grammar_structures;
DROP TABLE IF EXISTS word_form_pronunciations;
DROP TABLE IF EXISTS word_forms;
DROP TABLE IF EXISTS word_pos;
DROP TABLE IF EXISTS word_sense_groups;
DROP TABLE IF EXISTS words;
