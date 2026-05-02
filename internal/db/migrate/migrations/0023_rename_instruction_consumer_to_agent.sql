UPDATE tasks SET instructions = replace(instructions, '"consumer":', '"agent":')
WHERE instructions LIKE '%"consumer":%';
