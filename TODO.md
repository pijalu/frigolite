 Review the current state of the project and tests - the key element is to have all original sqlite3 tests working - perform complete research - original sqlite code is available in:
 /Users/muaddib/dev/sqlite

 The key elements should be:
 1/ Ensure all sqlite grammar is implemented
 2/ Categorize all tests into priorities - CRUD sql should be 1st priorities then specific sqlite supports like FTS, pragmas,...
 3/ Each category should be split into sub categories - create / insert / select / subselects / delete to match level of functionalities - Create specific tests to tests before trying to run
 sqlite tests to validate functionalities / unearth TCL transpiler issues instead of frigolite - original sqlite3 test have to be tested completly - no shortcut - the functional aspect must be
 preserved
 4/ A detailled plan must be written for implementations - each sub step should include a commit and update of the plan to ensure correct follow up
 5/ The plan should use goals to schedule and ensure goals are run with new context to limit cost - each sub items should have it's own sub-plan
 6/ Write the detailed plans - do not execute it
